package itest

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/rand"
	"google.golang.org/grpc/grpclog"
)

const (
	// defaultSplitTranches is the default number of tranches we split the
	// test cases into.
	defaultSplitTranches uint = 1

	// defaultRunTranche is the default index of the test cases tranche that
	// we run.
	defaultRunTranche uint = 0

	defaultTimeout = wait.DefaultTimeout
	itestLndBinary = "../lnd-itest"

	// TODO(yy): remove the following defined constants and put them in the
	// specific tests where they are used?
	testFeeBase    = 1e+6
	anchorSize     = 330
	defaultCSV     = node.DefaultCSV
	noFeeLimitMsat = math.MaxInt64

	AddrTypeWitnessPubkeyHash = lnrpc.AddressType_WITNESS_PUBKEY_HASH
	AddrTypeNestedPubkeyHash  = lnrpc.AddressType_NESTED_PUBKEY_HASH
	AddrTypeTaprootPubkey     = lnrpc.AddressType_TAPROOT_PUBKEY
)

var (
	harnessNetParams = &chaincfg.RegressionNetParams

	// testCasesSplitParts is the number of tranches the test cases should
	// be split into. By default this is set to 1, so no splitting happens.
	// If this value is increased, then the -runtranche flag must be
	// specified as well to indicate which part should be run in the current
	// invocation.
	testCasesSplitTranches = flag.Uint(
		"splittranches", defaultSplitTranches, "split the test cases "+
			"in this many tranches and run the tranche at "+
			"0-based index specified by the -runtranche flag",
	)

	// shuffleSeedFlag is the source of randomness used to shuffle the test
	// cases. If not specified, the test cases won't be shuffled.
	shuffleSeedFlag = flag.Uint64(
		"shuffleseed", 0, "if set, shuffles the test cases using this "+
			"as the source of randomness",
	)

	// testCasesRunTranche is the 0-based index of the split test cases
	// tranche to run in the current invocation.
	testCasesRunTranche = flag.Uint(
		"runtranche", defaultRunTranche, "run the tranche of the "+
			"split test cases with the given (0-based) index",
	)

	// dbBackendFlag specifies the backend to use.
	dbBackendFlag = flag.String("dbbackend", "bbolt", "Database backend "+
		"(bbolt, etcd, postgres)")

	nativeSQLFlag = flag.Bool("nativesql", false, "Database backend to "+
		"use native SQL when applicable (only for sqlite and postgres")

	// lndExecutable is the full path to the lnd binary.
	lndExecutable = flag.String(
		"lndexec", itestLndBinary, "full path to lnd binary",
	)
)

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programmatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	// If no tests are registered, then we can exit early.
	if len(allTestCases) == 0 {
		t.Skip("integration tests not selected with flag 'integration'")
	}

	// Get the test cases to be run in this tranche.
	testCases, trancheIndex, trancheOffset := getTestCaseSplitTranche()

	// Create a simple fee service.
	feeService := lntest.NewFeeService(t)

	// Get the binary path and setup the harness test.
	binary := getLndBinary(t)
	harnessTest := lntest.SetupHarness(
		t, binary, *dbBackendFlag, *nativeSQLFlag, feeService,
	)
	defer harnessTest.Stop()

	// Get the current block height.
	height := harnessTest.CurrentHeight()

	// Run the subset of the test cases selected in this tranche.
	for idx, testCase := range testCases {
		testCase := testCase
		name := fmt.Sprintf("tranche%02d/%02d-of-%d/%s/%s",
			trancheIndex, trancheOffset+uint(idx)+1,
			len(allTestCases), harnessTest.ChainBackendName(),
			testCase.Name)

		success := t.Run(name, func(t1 *testing.T) {
			// Create a separate harness test for the testcase to
			// avoid overwriting the external harness test that is
			// tied to the parent test.
			ht := harnessTest.Subtest(t1)
			ht.SetTestName(testCase.Name)

			ht.RunTestCase(testCase)
		})

		// Stop at the first failure. Mimic behavior of original test
		// framework.
		if !success {
			// Log failure time to help relate the lnd logs to the
			// failure.
			t.Logf("Failure time: %v", time.Now().Format(
				"2006-01-02 15:04:05.000",
			))
			break
		}
	}

	//nolint:forbidigo
	fmt.Printf("=========> tranche %v finished, tested %d cases, mined "+
		"blocks: %d\n", trancheIndex, len(testCases),
		harnessTest.CurrentHeight()-height)
}

// maybeShuffleTestCases shuffles the test cases if the flag `shuffleseed` is
// set and not 0. In parallel tests we want to shuffle the test cases so they
// are executed in a random order. This is done to even out the blocks mined in
// each test tranche so they can run faster.
//
// NOTE: Because the parallel tests are initialized with the same seed (job
// ID), they will always have the same order.
func maybeShuffleTestCases() {
	// Exit if not set.
	if shuffleSeedFlag == nil {
		return
	}

	// Exit if set to 0.
	if *shuffleSeedFlag == 0 {
		return
	}

	// Init the seed and shuffle the test cases.
	rand.Seed(*shuffleSeedFlag)
	rand.Shuffle(len(allTestCases), func(i, j int) {
		allTestCases[i], allTestCases[j] =
			allTestCases[j], allTestCases[i]
	})
}

// createIndices divides the number of test cases into pairs of indices that
// specify the start and end of a tranche.
func createIndices(numCases, numTranches uint) [][2]uint {
	// Calculate base value and remainder.
	base := numCases / numTranches
	remainder := numCases % numTranches

	// Generate indices.
	indices := make([][2]uint, numTranches)
	start := uint(0)

	for i := uint(0); i < numTranches; i++ {
		end := start + base
		if i < remainder {
			// Add one for the remainder.
			end++
		}
		indices[i] = [2]uint{start, end}
		start = end
	}

	return indices
}

// getTestCaseSplitTranche returns the sub slice of the test cases that should
// be run as the current split tranche as well as the index and slice offset of
// the tranche.
func getTestCaseSplitTranche() ([]*lntest.TestCase, uint, uint) {
	numTranches := defaultSplitTranches
	if testCasesSplitTranches != nil {
		numTranches = *testCasesSplitTranches
	}
	runTranche := defaultRunTranche
	if testCasesRunTranche != nil {
		runTranche = *testCasesRunTranche
	}

	// There's a special flake-hunt mode where we run the same test multiple
	// times in parallel. In that case the tranche index is equal to the
	// thread ID, but we need to actually run all tests for the regex
	// selection to work.
	threadID := runTranche
	if numTranches == 1 {
		runTranche = 0
	}

	// Shuffle the test cases if the `shuffleseed` flag is set.
	maybeShuffleTestCases()

	numCases := uint(len(allTestCases))
	indices := createIndices(numCases, numTranches)
	index := indices[runTranche]
	trancheOffset, trancheEnd := index[0], index[1]

	return allTestCases[trancheOffset:trancheEnd], threadID,
		trancheOffset
}

func getLndBinary(t *testing.T) string {
	binary := itestLndBinary
	lndExec := ""
	if lndExecutable != nil && *lndExecutable != "" {
		lndExec = *lndExecutable
	}
	if lndExec == "" && runtime.GOOS == "windows" {
		// Windows (even in a bash like environment like git bash as on
		// Travis) doesn't seem to like relative paths to exe files...
		currentDir, err := os.Getwd()
		require.NoError(t, err, "unable to get working directory")

		targetPath := filepath.Join(currentDir, "../../lnd-itest.exe")
		binary, err = filepath.Abs(targetPath)
		require.NoError(t, err, "unable to get absolute path")
	} else if lndExec != "" {
		binary = lndExec
	}

	return binary
}

// isWindowsOS returns true if the test is running on a Windows OS.
func isWindowsOS() bool {
	return runtime.GOOS == "windows"
}

func init() {
	// Before we start any node, we need to make sure that any btcd node
	// that is started through the RPC harness uses a unique port as well
	// to avoid any port collisions.
	rpctest.ListenAddressGenerator =
		port.GenerateSystemUniqueListenerAddresses

	// Swap out grpc's default logger with our fake logger which drops the
	// statements on the floor.
	fakeLogger := grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard)
	grpclog.SetLoggerV2(fakeLogger)
}
