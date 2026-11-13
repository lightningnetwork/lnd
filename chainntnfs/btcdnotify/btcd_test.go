//go:build dev
// +build dev

package btcdnotify

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntest/unittest"
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

// setUpNotifier is a helper function to start a new notifier backed by a btcd
// driver.
func setUpNotifier(t *testing.T, h *rpctest.Harness) *BtcdNotifier {
	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	rpcCfg := h.RPCConfig()
	notifier, err := New(
		&rpcCfg, unittest.NetParams, hintCache, hintCache, blockCache,
	)
	require.NoError(t, err, "unable to create notifier")
	if err := notifier.Start(); err != nil {
		t.Fatalf("unable to start notifier: %v", err)
	}
	t.Cleanup(func() {
		require.NoError(t, notifier.Stop())
	})

	return notifier
}

// TestHistoricalConfDetailsTxIndex ensures that we correctly retrieve
// historical confirmation details using the backend node's txindex.
func TestHistoricalConfDetailsTxIndex(t *testing.T) {
	t.Parallel()

	harness := unittest.NewMiner(
		t, unittest.NetParams, []string{"--txindex"}, true, 25,
	)

	notifier := setUpNotifier(t, harness)

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

	// Now, we'll create a test transaction and attempt to retrieve its
	// confirmation details.
	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(harness)
	require.NoError(t, err, "unable to create tx")
	if err := chainntnfs.WaitForMempoolTx(harness, txid); err != nil {
		t.Fatalf("unable to find tx in the mempool: %v", err)
	}
	confReq, err := chainntnfs.NewConfRequest(txid, pkScript)
	require.NoError(t, err, "unable to create conf request")

	// The transaction should be found in the mempool at this point.
	_, txStatus, err = notifier.historicalConfDetails(confReq, 0, 0)
	require.NoError(t, err, "unable to retrieve historical conf details")

	// Since it has yet to be included in a block, it should have been found
	// within the mempool.
	switch txStatus {
	case chainntnfs.TxFoundMempool:
	default:
		t.Fatalf("should have found the transaction within the "+
			"mempool, but did not: %v", txStatus)
	}

	// We'll now confirm this transaction and re-attempt to retrieve its
	// confirmation details.
	if _, err := harness.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

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
	t.Parallel()

	harness := unittest.NewMiner(t, unittest.NetParams, nil, true, 25)

	notifier := setUpNotifier(t, harness)

	// Since the node has its txindex disabled, we fall back to scanning the
	// chain manually. A transaction unknown to the network should not be
	// found.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	unknownConfReq, err := chainntnfs.NewConfRequest(&unknownHash, testScript)
	require.NoError(t, err, "unable to create conf request")
	_, txStatus, err := notifier.historicalConfDetails(unknownConfReq, 0, 0)
	require.NoError(t, err, "unable to retrieve historical conf details")

	switch txStatus {
	case chainntnfs.TxNotFoundManually:
	case chainntnfs.TxNotFoundIndex:
		t.Fatal("should have proceeded with fallback method, but did not")
	default:
		t.Fatal("should not have found non-existent transaction, but did")
	}

	// Now, we'll create a test transaction and attempt to retrieve its
	// confirmation details. We'll note its broadcast height to use as the
	// height hint when manually scanning the chain.
	_, currentHeight, err := harness.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve current height")

	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(harness)
	require.NoError(t, err, "unable to create tx")
	if err := chainntnfs.WaitForMempoolTx(harness, txid); err != nil {
		t.Fatalf("unable to find tx in the mempool: %v", err)
	}
	confReq, err := chainntnfs.NewConfRequest(txid, pkScript)
	require.NoError(t, err, "unable to create conf request")

	_, txStatus, err = notifier.historicalConfDetails(confReq, 0, 0)
	require.NoError(t, err, "unable to retrieve historical conf details")

	// Since it has yet to be included in a block, it should have been found
	// within the mempool.
	if txStatus != chainntnfs.TxFoundMempool {
		t.Fatal("should have found the transaction within the " +
			"mempool, but did not")
	}

	// We'll now confirm this transaction and re-attempt to retrieve its
	// confirmation details.
	if _, err := harness.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	_, txStatus, err = notifier.historicalConfDetails(
		confReq, uint32(currentHeight), uint32(currentHeight)+1,
	)
	require.NoError(t, err, "unable to retrieve historical conf details")

	// Since the backend node's txindex is disabled and the transaction has
	// confirmed, we should be able to find it by falling back to scanning
	// the chain manually.
	if txStatus != chainntnfs.TxFoundManually {
		t.Fatal("should have found the transaction by manually " +
			"scanning the chain, but did not")
	}
}

// TestHistoricalSpendDetails ensures that we correctly retrieve historical
// spend details of outpoints and output scripts by manually scanning the
// chain.
func TestHistoricalSpendDetails(t *testing.T) {
	t.Parallel()

	harness := unittest.NewMiner(t, unittest.NetParams, nil, true, 25)

	notifier := setUpNotifier(t, harness)

	// We'll create an output and note the height it confirmed at to use as
	// the start of the scanned range.
	outpoint, output, privKey := chainntnfs.CreateSpendableOutput(
		t, harness,
	)
	_, startHeight, err := harness.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve current height")

	opReq, err := chainntnfs.NewSpendRequest(outpoint, output.PkScript)
	require.NoError(t, err, "unable to create spend request")
	scriptReq, err := chainntnfs.NewSpendRequest(nil, output.PkScript)
	require.NoError(t, err, "unable to create spend request")

	// Since the output has yet to be spent, no spend details should be
	// found for either request.
	for _, req := range []chainntnfs.SpendRequest{opReq, scriptReq} {
		details, err := notifier.historicalSpendDetails(
			req, uint32(startHeight), uint32(startHeight),
		)
		require.NoError(t, err, "unable to retrieve spend details")
		require.Nil(t, details)
	}

	// We'll now spend the output, confirm the spend and bury it under a
	// few more blocks.
	spendTx := chainntnfs.CreateSpendTx(t, outpoint, output, privKey)
	spendTxid, err := harness.Client.SendRawTransaction(spendTx, true)
	require.NoError(t, err, "unable to broadcast tx")
	err = chainntnfs.WaitForMempoolTx(harness, spendTxid)
	require.NoError(t, err, "tx not relayed to miner")

	_, err = harness.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")
	_, spendHeight, err := harness.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve current height")

	_, err = harness.Client.Generate(3)
	require.NoError(t, err, "unable to generate blocks")
	_, endHeight, err := harness.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve current height")

	// The spend should now be found for both requests.
	for _, req := range []chainntnfs.SpendRequest{opReq, scriptReq} {
		details, err := notifier.historicalSpendDetails(
			req, uint32(startHeight), uint32(endHeight),
		)
		require.NoError(t, err, "unable to retrieve spend details")
		require.NotNil(t, details)
		require.Equal(t, *outpoint, *details.SpentOutPoint)
		require.Equal(t, *spendTxid, *details.SpenderTxHash)
		require.Equal(t, uint32(0), details.SpenderInputIndex)
		require.Equal(t, spendHeight, details.SpendingHeight)
	}

	// A range that ends before the spend should not find it.
	details, err := notifier.historicalSpendDetails(
		scriptReq, uint32(startHeight), uint32(spendHeight)-1,
	)
	require.NoError(t, err, "unable to retrieve spend details")
	require.Nil(t, details)
}

// TestHistoricalSpendDispatchUnspent ensures that a historical spend dispatch
// for an output script that has not been spent completes, so that the spend
// hint of the request is committed.
func TestHistoricalSpendDispatchUnspent(t *testing.T) {
	t.Parallel()

	harness := unittest.NewMiner(t, unittest.NetParams, nil, true, 25)

	notifier := setUpNotifier(t, harness)

	epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err, "unable to register for block epoch")
	defer epochClient.Cancel()

	_, output, _ := chainntnfs.CreateSpendableOutput(t, harness)
	_, height, err := harness.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve current height")

	// Wait for the notifier to process the output's block, so that
	// registering with its height as the hint dispatches a historical
	// rescan.
	timeout := time.After(10 * time.Second)
	for caughtUp := false; !caughtUp; {
		select {
		case epoch := <-epochClient.Epochs:
			caughtUp = epoch.Height == height

		case <-timeout:
			t.Fatalf("notifier did not reach height %d", height)
		}
	}

	_, err = notifier.RegisterSpendNtfn(
		nil, output.PkScript, uint32(height),
	)
	require.NoError(t, err, "unable to register spend ntfn")

	req, err := chainntnfs.NewSpendRequest(nil, output.PkScript)
	require.NoError(t, err, "unable to create spend request")

	// Once the historical dispatch completes without finding a spend, the
	// spend hint of the request is committed at the current height.
	require.Eventually(t, func() bool {
		hint, err := notifier.spendHintCache.QuerySpendHint(req)
		return err == nil && hint == uint32(height)
	}, 10*time.Second, 50*time.Millisecond)
}
