package lntest

import (
	"context"
	"os"
	"testing"

	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// SetupHarness creates a new HarnessTest with a series of setups such that the
// instance is ready for usage. The setups are,
// 1. create the directories to hold lnd files.
// 2. start a btcd miner.
// 3. start a chain backend(btcd, bitcoind, or neutrino).
// 4. connect the miner and the chain backend.
// 5. start the HarnessTest.
func SetupHarness(t *testing.T, binaryPath, dbBackendName string,
	feeService WebFeeService) *HarnessTest {

	t.Log("Setting up HarnessTest...")

	// Parse testing flags that influence our test execution.
	logDir := node.GetLogDir()
	require.NoError(t, os.MkdirAll(logDir, 0700), "create log dir failed")

	// Parse database backend
	dbBackend := prepareDBBackend(t, dbBackendName)

	// Create a new HarnessTest.
	ht := NewHarnessTest(t, binaryPath, feeService, dbBackend)

	// Init the miner.
	t.Log("Prepare the miner and mine blocks to activate segwit...")
	miner := prepareMiner(ht.runCtx, ht.T)

	// Start a chain backend.
	chainBackend, cleanUp := prepareChainBackend(t, miner.P2PAddress())
	ht.stopChainBackend = cleanUp

	// Connect our chainBackend to our miner.
	t.Log("Connecting the miner with the chain backend...")
	require.NoError(t, chainBackend.ConnectMiner(), "connect miner")

	// Start the HarnessTest with the chainBackend and miner.
	ht.Start(chainBackend, miner)

	return ht
}

// prepareMiner creates an instance of the btcd's rpctest.Harness that will act
// as the miner for all tests. This will be used to fund the wallets of the
// nodes within the test network and to drive blockchain related events within
// the network. Revert the default setting of accepting non-standard
// transactions on simnet to reject them. Transactions on the lightning network
// should always be standard to get better guarantees of getting included in to
// blocks.
func prepareMiner(ctxt context.Context, t *testing.T) *HarnessMiner {
	miner := NewMiner(ctxt, t)

	// Before we start anything, we want to overwrite some of the
	// connection settings to make the tests more robust. We might need to
	// restart the miner while there are already blocks present, which will
	// take a bit longer than the 1 second the default settings amount to.
	// Doubling both values will give us retries up to 4 seconds.
	miner.MaxConnRetries = rpctest.DefaultMaxConnectionRetries * 2
	miner.ConnectionRetryTimeout = rpctest.DefaultConnectionRetryTimeout * 2

	// Set up miner and connect chain backend to it.
	require.NoError(t, miner.SetUp(true, 50))
	require.NoError(t, miner.Client.NotifyNewTransactions(false))

	// Next mine enough blocks in order for segwit and the CSV package
	// soft-fork to activate on SimNet.
	numBlocks := harnessNetParams.MinerConfirmationWindow * 2
	miner.GenerateBlocks(numBlocks)

	return miner
}

// prepareChainBackend creates a new chain backend.
func prepareChainBackend(t *testing.T,
	minerAddr string) (node.BackendConfig, func()) {

	chainBackend, cleanUp, err := NewBackend(
		minerAddr, harnessNetParams,
	)
	require.NoError(t, err, "new backend")

	return chainBackend, func() {
		require.NoError(t, cleanUp(), "cleanup")
	}
}

// prepareDBBackend parses a DatabaseBackend based on the name given.
func prepareDBBackend(t *testing.T,
	dbBackendName string) node.DatabaseBackend {

	var dbBackend node.DatabaseBackend
	switch dbBackendName {
	case "bbolt":
		dbBackend = node.BackendBbolt

	case "etcd":
		dbBackend = node.BackendEtcd

	case "postgres":
		dbBackend = node.BackendPostgres

	case "sqlite":
		dbBackend = node.BackendSqlite

	default:
		require.Fail(t, "unknown db backend")
	}

	return dbBackend
}
