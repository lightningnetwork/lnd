package itest

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

const (
	// defaultSplitTranches is the default number of tranches we split the
	// test cases into.
	defaultSplitTranches uint = 1

	// defaultRunTranche is the default index of the test cases tranche that
	// we run.
	defaultRunTranche uint = 0
)

var (
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

	// testCasesRunTranche is the 0-based index of the split test cases
	// tranche to run in the current invocation.
	testCasesRunTranche = flag.Uint(
		"runtranche", defaultRunTranche, "run the tranche of the "+
			"split test cases with the given (0-based) index",
	)

	// dbBackendFlag specifies the backend to use.
	dbBackendFlag = flag.String("dbbackend", "bbolt", "Database backend "+
		"(bbolt, etcd, postgres)")
)

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

	numCases := uint(len(allTestCases))
	testsPerTranche := numCases / numTranches
	trancheOffset := runTranche * testsPerTranche
	trancheEnd := trancheOffset + testsPerTranche
	if trancheEnd > numCases || runTranche == numTranches-1 {
		trancheEnd = numCases
	}

	return allTestCases[trancheOffset:trancheEnd], threadID, trancheOffset
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

// prepareMiner creates an instance of the btcd's rpctest.Harness that will act
// as the miner for all tests. This will be used to fund the wallets of the
// nodes within the test network and to drive blockchain related events within
// the network. Revert the default setting of accepting non-standard
// transactions on simnet to reject them. Transactions on the lightning network
// should always be standard to get better guarantees of getting included in to
// blocks.
func prepareMiner(t *testing.T) (*lntest.HarnessMiner, func()) {
	miner, err := lntest.NewMiner()
	require.NoError(t, err, "failed to create new miner")
	cleanUp := func() {
		require.NoError(t, miner.Stop(), "failed to stop miner")
	}

	// Before we start anything, we want to overwrite some of the connection
	// settings to make the tests more robust. We might need to restart the
	// miner while there are already blocks present, which will take a bit
	// longer than the 1 second the default settings amount to. Doubling
	// both values will give us retries up to 4 seconds.
	miner.MaxConnRetries = rpctest.DefaultMaxConnectionRetries * 2
	miner.ConnectionRetryTimeout = rpctest.DefaultConnectionRetryTimeout * 2

	// Set up miner and connect chain backend to it.
	require.NoError(t, miner.SetUp(true, 50))
	require.NoError(t, miner.Client.NotifyNewTransactions(false))

	// Next mine enough blocks in order for segwit and the CSV package
	// soft-fork to activate on SimNet.
	numBlocks := harnessNetParams.MinerConfirmationWindow * 2
	_, err = miner.Client.Generate(numBlocks)
	require.NoError(t, err, "unable to generate blocks")

	return miner, cleanUp
}

func prepareChainBackend(t *testing.T,
	minerAddr string) (lntest.BackendConfig, func()) {

	chainBackend, cleanUp, err := lntest.NewBackend(
		minerAddr, harnessNetParams,
	)
	require.NoError(t, err, "new backend")

	return chainBackend, func() {
		require.NoError(t, cleanUp(), "cleanup")
	}
}

func prepareDbBackend(t *testing.T) lntest.DatabaseBackend {
	var dbBackend lntest.DatabaseBackend
	switch *dbBackendFlag {
	case "bbolt":
		dbBackend = lntest.BackendBbolt

	case "etcd":
		dbBackend = lntest.BackendEtcd

	case "postgres":
		dbBackend = lntest.BackendPostgres

	default:
		require.Fail(t, "unknown db backend")
	}

	return dbBackend
}

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programmatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	// If no tests are registered, then we can exit early.
	if len(allTestCases) == 0 {
		t.Skip("integration tests not selected with flag 'rpctest'")
	}

	// Parse testing flags that influence our test execution.
	logDir := lntest.GetLogDir()
	require.NoError(t, os.MkdirAll(logDir, 0700))
	testCases, trancheIndex, trancheOffset := getTestCaseSplitTranche()
	lntest.ApplyPortOffset(uint32(trancheIndex) * 1000)

	// Before we start any node, we need to make sure that any btcd node
	// that is started through the RPC harness uses a unique port as well
	// to avoid any port collisions.
	rpctest.ListenAddressGenerator = lntest.GenerateBtcdListenerAddresses

	// We will also connect it to our chain backend.
	miner, cleanMiner := prepareMiner(t)
	defer cleanMiner()

	// Start a chain backend.
	chainBackend, cleanUp := prepareChainBackend(t, miner.P2PAddress())
	defer cleanUp()

	// Connect our chainBackend to our miner.
	require.NoError(t, chainBackend.ConnectMiner(), "connect miner")

	// Parse database backend
	dbBackend := prepareDbBackend(t)

	// Now we can set up our test harness (LND instance), with the chain
	// backend we just created.
	binary := getLndBinary(t)
	lndHarness, err := lntest.NewNetworkHarness(
		miner, chainBackend, binary, dbBackend,
	)
	require.NoError(t, err, "unable to create lightning network harness")
	defer lndHarness.Stop(t)

	// Spawn a new goroutine to watch for any fatal errors that any of the
	// running lnd processes encounter. If an error occurs, then the test
	// case should naturally as a result and we log the server error here to
	// help debug.
	go func() {
		for {
			select {
			case err, more := <-lndHarness.ProcessErrors():
				if !more {
					return
				}
				t.Logf("lnd finished with error (stderr):\n%v",
					err)
			}
		}
	}()

	// With the btcd harness created, we can now complete the
	// initialization of the network. args - list of lnd arguments,
	// example: "--debuglevel=debug"
	// TODO(roasbeef): create master balanced channel with all the monies?
	aliceBobArgs := []string{
		"--default-remote-max-htlcs=483",
		"--dust-threshold=5000000",
	}

	// Create the harness test and setup two nodes, Alice and Bob, which
	// will be alive and shared among all the test cases.
	harnessTest := lntest.NewHarnessTest(t, lndHarness)
	harnessTest.SetUp(aliceBobArgs)

	// Run the subset of the test cases selected in this tranche.
	for idx, testCase := range testCases {
		testCase := testCase
		name := fmt.Sprintf("tranche%02d/%02d-of-%d/%s/%s",
			trancheIndex, trancheOffset+uint(idx)+1,
			len(allTestCases), chainBackend.Name(), testCase.Name)

		success := t.Run(name, func(t1 *testing.T) {
			// Create a separate harness test for the testcase to
			// avoid overwriting the external harness test that is
			// tied to the parent test.
			ht, cleanup := harnessTest.Subtest(t1)
			defer cleanup()

			cleanTestCaseName := strings.ReplaceAll(
				testCase.Name, " ", "_",
			)

			ht.SetTestName(cleanTestCaseName)

			logLine := fmt.Sprintf(
				"STARTING ============ %v ============\n",
				testCase.Name,
			)

			ht.Alice.AddToLogf(logLine)
			ht.Bob.AddToLogf(logLine)

			// Start every test with the default static fee
			// estimate.
			ht.SetFeeEstimate(12500)
			ht.EnsureConnected(ht.Alice, ht.Bob)

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

	_, height := harnessTest.GetBestBlock()
	t.Logf("=========> tests finished for tranche: %v, tested %d "+
		"cases, end height: %d\n", trancheIndex, len(testCases), height)
}
