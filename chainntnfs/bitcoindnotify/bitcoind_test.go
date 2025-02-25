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
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntest/unittest"
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

	db := channeldb.OpenForTesting(t, t.TempDir())

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
		bitcoindConn, unittest.NetParams, spendHintCache,
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

		t.Logf("miner height=%v, bitcoind height=%v", minerHeight,
			bitcoindHeight)

		if bitcoindHeight == minerHeight {
			return uint32(bitcoindHeight)
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-timeout:
			t.Fatalf("timed out in syncNotifierWithMiner, got "+
				"err=%v, minerHeight=%v, bitcoindHeight=%v",
				err, minerHeight, bitcoindHeight)
		}

		// Get the num of connections the miner has. We expect it to
		// have at least one connection with the chain backend.
		count, err := miner.Client.GetConnectionCount()
		require.NoError(t, err)
		if count != 0 {
			continue
		}

		// Reconnect the miner and the chain backend.
		//
		// NOTE: The connection should have been made before we perform
		// the `syncNotifierWithMiner`. However, due to an unknown
		// reason, the miner may refuse to process the inbound
		// connection made by the bitcoind node, causing the connection
		// to fail. It's possible there's a bug in the handshake between
		// the two nodes.
		//
		// A normal flow is, bitcoind starts a v2 handshake flow, which
		// btcd will fail and disconnect. Upon seeing this
		// disconnection, bitcoind will try a v1 handshake and succeeds.
		// The failed flow is, upon seeing the v2 handshake, btcd
		// doesn't seem to perform the disconnect. Instead an EOF
		// websocket error is found.
		//
		// TODO(yy): Fix the above bug in `btcd`. This can be reproduced
		// using `make flakehunter-unit pkg=$pkg case=$case`, with,
		// `case=TestHistoricalConfDetailsNoTxIndex/rpc_polling_enabled`
		// `pkg=chainntnfs/bitcoindnotify`.
		// Also need to modify the temp dir logic so we can save the
		// debug logs.
		// This bug is likely to be fixed when we implement the
		// encrypted p2p conn, or when we properly fix the shutdown
		// issues in all our RPC conns.
		t.Log("Expected to the chain backend to have one conn with " +
			"the miner, instead it's disconnected!")

		// We now ask the miner to add the chain backend back.
		host := fmt.Sprintf(
			"127.0.0.1:%s", notifier.chainParams.DefaultPort,
		)

		// NOTE:AddNode must take a host that has the format
		// `host:port`, otherwise the default port will be used. Check
		// `normalizeAddress` in btcd for details.
		err = miner.Client.AddNode(host, rpcclient.ANAdd)
		require.NoError(t, err, "Failed to connect miner to the chain "+
			"backend")
	}
}

// TestHistoricalConfDetailsTxIndex ensures that we correctly retrieve
// historical confirmation details using the backend node's txindex.
func TestHistoricalConfDetailsTxIndex(t *testing.T) {
	success := t.Run("rpc polling enabled", func(st *testing.T) {
		st.Parallel()
		testHistoricalConfDetailsTxIndex(st, true)
	})

	if !success {
		return
	}

	t.Run("rpc polling disabled", func(st *testing.T) {
		st.Parallel()
		testHistoricalConfDetailsTxIndex(st, false)
	})
}

func testHistoricalConfDetailsTxIndex(t *testing.T, rpcPolling bool) {
	miner := unittest.NewMiner(
		t, unittest.NetParams, []string{"--txindex"}, true, 25,
	)

	bitcoindConn := unittest.NewBitcoindBackend(
		t, unittest.NetParams, miner, true, rpcPolling,
	)

	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	notifier := setUpNotifier(
		t, bitcoindConn, hintCache, hintCache, blockCache,
	)

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
	success := t.Run("rpc polling enabled", func(st *testing.T) {
		st.Parallel()
		testHistoricalConfDetailsNoTxIndex(st, true)
	})

	if !success {
		return
	}

	t.Run("rpc polling disabled", func(st *testing.T) {
		st.Parallel()
		testHistoricalConfDetailsNoTxIndex(st, false)
	})
}

func testHistoricalConfDetailsNoTxIndex(t *testing.T, rpcpolling bool) {
	miner := unittest.NewMiner(t, unittest.NetParams, nil, true, 25)

	bitcoindConn := unittest.NewBitcoindBackend(
		t, unittest.NetParams, miner, false, rpcpolling,
	)

	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	notifier := setUpNotifier(
		t, bitcoindConn, hintCache, hintCache, blockCache,
	)

	// Since the node has its txindex disabled, we fall back to scanning the
	// chain manually. A transaction unknown to the network should not be
	// found.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	unknownConfReq, err := chainntnfs.NewConfRequest(&unknownHash, testScript)
	require.NoError(t, err, "unable to create conf request")

	// Get the current best height.
	_, broadcastHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve miner's current height")

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
