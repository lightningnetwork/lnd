//go:build dev
// +build dev

package bitcoindnotify

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

var (
	testScript = []byte{
		// OP_HASH160
		0xA9,
		// OP_DATA_20
		0x14,
		// <20-byte hash>
		0xec, 0x6f, 0x7a, 0x5a, 0xa8, 0xf2, 0xb1, 0x0c, 0xa5, 0x15,
		0x04, 0x52, 0x3a, 0x60, 0xd4, 0x03, 0x06, 0xf6, 0x96, 0xcd,
		// OP_EQUAL
		0x87,
	}
)

func initHintCache(t *testing.T) *channeldb.HeightHintCache {
	t.Helper()

	db, err := channeldb.Open(t.TempDir())
	require.NoError(t, err, "unable to create db")
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	testCfg := channeldb.CacheConfig{
		QueryDisable: false,
	}
	hintCache, err := channeldb.NewHeightHintCache(testCfg, db.Backend)
	require.NoError(t, err, "unable to create hint cache")

	return hintCache
}

// setUpNotifier is a helper function to start a new notifier backed by a
// bitcoind driver.
func setUpNotifier(t *testing.T, bitcoindConn *chain.BitcoindConn,
	spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache,
	blockCache *blockcache.BlockCache) *BitcoindNotifier {

	t.Helper()

	notifier := New(
		bitcoindConn, chainntnfs.NetParams, spendHintCache,
		confirmHintCache, blockCache,
	)
	if err := notifier.Start(); err != nil {
		t.Fatalf("unable to start notifier: %v", err)
	}
	t.Cleanup(func() {
		require.NoError(t, notifier.Stop())
	})

	return notifier
}

// syncNotifierWithMiner is a helper method that attempts to wait until the
// notifier is synced (in terms of the chain) with the miner.
func syncNotifierWithMiner(t *testing.T, notifier *BitcoindNotifier,
	miner *rpctest.Harness) uint32 {

	t.Helper()

	_, minerHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve miner's current height")

	timeout := time.After(10 * time.Second)
	for {
		_, bitcoindHeight, err := notifier.chainConn.GetBestBlock()
		if err != nil {
			t.Fatalf("unable to retrieve bitcoind's current "+
				"height: %v", err)
		}

		if bitcoindHeight == minerHeight {
			return uint32(bitcoindHeight)
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-timeout:
			t.Fatalf("timed out waiting to sync notifier")
		}
	}
}

// TestHistoricalConfDetailsTxIndex ensures that we correctly retrieve
// historical confirmation details using the backend node's txindex.
func TestHistoricalConfDetailsTxIndex(t *testing.T) {
	t.Run("rpc polling enabled", func(st *testing.T) {
		st.Parallel()
		testHistoricalConfDetailsTxIndex(st, true)
	})

	t.Run("rpc polling disabled", func(st *testing.T) {
		st.Parallel()
		testHistoricalConfDetailsTxIndex(st, false)
	})
}

func testHistoricalConfDetailsTxIndex(t *testing.T, rpcPolling bool) {
	miner := chainntnfs.NewMiner(
		t, []string{"--txindex"}, true, 25,
	)

	bitcoindConn := chainntnfs.NewBitcoindBackend(
		t, miner.P2PAddress(), true, rpcPolling,
	)

	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	notifier := setUpNotifier(
		t, bitcoindConn, hintCache, hintCache, blockCache,
	)

	syncNotifierWithMiner(t, notifier, miner)

	// A transaction unknown to the node should not be found within the
	// txindex even if it is enabled, so we should not proceed with any
	// fallback methods.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	unknownConfReq, err := chainntnfs.NewConfRequest(&unknownHash, testScript)
	require.NoError(t, err, "unable to create conf request")
	_, txStatus, err := notifier.historicalConfDetails(unknownConfReq, 0, 0)
	require.NoError(t, err, "unable to retrieve historical conf details")

	switch txStatus {
	case chainntnfs.TxNotFoundIndex:
	case chainntnfs.TxNotFoundManually:
		t.Fatal("should not have proceeded with fallback method, but did")
	default:
		t.Fatal("should not have found non-existent transaction, but did")
	}

	// Now, we'll create a test transaction, confirm it, and attempt to
	// retrieve its confirmation details.
	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatal(err)
	}
	confReq, err := chainntnfs.NewConfRequest(txid, pkScript)
	require.NoError(t, err, "unable to create conf request")

	// The transaction should be found in the mempool at this point. We use
	// wait here to give miner some time to propagate the tx to our node.
	err = wait.NoError(func() error {
		// The call should return no error.
		_, txStatus, err = notifier.historicalConfDetails(confReq, 0, 0)
		require.NoError(t, err)

		if txStatus != chainntnfs.TxFoundMempool {
			return fmt.Errorf("cannot the tx in mempool, status "+
				"is: %v", txStatus)
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(t, err, "timeout waitinfg for historicalConfDetails")

	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Ensure the notifier and miner are synced to the same height to ensure
	// the txindex includes the transaction just mined.
	syncNotifierWithMiner(t, notifier, miner)

	_, txStatus, err = notifier.historicalConfDetails(confReq, 0, 0)
	require.NoError(t, err, "unable to retrieve historical conf details")

	// Since the backend node's txindex is enabled and the transaction has
	// confirmed, we should be able to retrieve it using the txindex.
	switch txStatus {
	case chainntnfs.TxFoundIndex:
	default:
		t.Fatal("should have found the transaction within the " +
			"txindex, but did not")
	}
}

// TestHistoricalConfDetailsNoTxIndex ensures that we correctly retrieve
// historical confirmation details using the set of fallback methods when the
// backend node's txindex is disabled.
func TestHistoricalConfDetailsNoTxIndex(t *testing.T) {
	t.Run("rpc polling enabled", func(st *testing.T) {
		st.Parallel()
		testHistoricalConfDetailsNoTxIndex(st, true)
	})

	t.Run("rpc polling disabled", func(st *testing.T) {
		st.Parallel()
		testHistoricalConfDetailsNoTxIndex(st, false)
	})
}

func testHistoricalConfDetailsNoTxIndex(t *testing.T, rpcpolling bool) {
	miner := chainntnfs.NewMiner(t, nil, true, 25)

	bitcoindConn := chainntnfs.NewBitcoindBackend(
		t, miner.P2PAddress(), false, rpcpolling,
	)

	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	notifier := setUpNotifier(t, bitcoindConn, hintCache, hintCache, blockCache)

	// Since the node has its txindex disabled, we fall back to scanning the
	// chain manually. A transaction unknown to the network should not be
	// found.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	unknownConfReq, err := chainntnfs.NewConfRequest(&unknownHash, testScript)
	require.NoError(t, err, "unable to create conf request")
	broadcastHeight := syncNotifierWithMiner(t, notifier, miner)
	_, txStatus, err := notifier.historicalConfDetails(
		unknownConfReq, uint32(broadcastHeight), uint32(broadcastHeight),
	)
	require.NoError(t, err, "unable to retrieve historical conf details")

	switch txStatus {
	case chainntnfs.TxNotFoundManually:
	case chainntnfs.TxNotFoundIndex:
		t.Fatal("should have proceeded with fallback method, but did not")
	default:
		t.Fatal("should not have found non-existent transaction, but did")
	}

	// Now, we'll create a test transaction and attempt to retrieve its
	// confirmation details. In order to fall back to manually scanning the
	// chain, the transaction must be in the chain and not contain any
	// unspent outputs. To ensure this, we'll create a transaction with only
	// one output, which we will manually spend. The backend node's
	// transaction index should also be disabled, which we've already
	// ensured above.
	outpoint, output, privKey := chainntnfs.CreateSpendableOutput(t, miner)
	spendTx := chainntnfs.CreateSpendTx(t, outpoint, output, privKey)
	spendTxHash, err := miner.Client.SendRawTransaction(spendTx, true)
	require.NoError(t, err, "unable to broadcast tx")
	if err := chainntnfs.WaitForMempoolTx(miner, spendTxHash); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}
	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Ensure the notifier and miner are synced to the same height to ensure
	// we can find the transaction when manually scanning the chain.
	confReq, err := chainntnfs.NewConfRequest(&outpoint.Hash, output.PkScript)
	require.NoError(t, err, "unable to create conf request")
	currentHeight := syncNotifierWithMiner(t, notifier, miner)
	_, txStatus, err = notifier.historicalConfDetails(
		confReq, uint32(broadcastHeight), uint32(currentHeight),
	)
	require.NoError(t, err, "unable to retrieve historical conf details")

	// Since the backend node's txindex is disabled and the transaction has
	// confirmed, we should be able to find it by falling back to scanning
	// the chain manually.
	switch txStatus {
	case chainntnfs.TxFoundManually:
	default:
		t.Fatal("should have found the transaction by manually " +
			"scanning the chain, but did not")
	}
}
