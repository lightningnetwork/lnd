//go:build dev
// +build dev

package btcdnotify

import (
	"bytes"
	"io/ioutil"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
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

func initHintCache(t *testing.T) *chainntnfs.HeightHintCache {
	t.Helper()

	tempDir, err := ioutil.TempDir("", "kek")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}
	db, err := channeldb.Open(tempDir)
	if err != nil {
		t.Fatalf("unable to create db: %v", err)
	}
	testCfg := chainntnfs.CacheConfig{
		QueryDisable: false,
	}
	hintCache, err := chainntnfs.NewHeightHintCache(testCfg, db.Backend)
	if err != nil {
		t.Fatalf("unable to create hint cache: %v", err)
	}

	return hintCache
}

// setUpNotifier is a helper function to start a new notifier backed by a btcd
// driver.
func setUpNotifier(t *testing.T, h *rpctest.Harness) *BtcdNotifier {
	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	rpcCfg := h.RPCConfig()
	notifier, err := New(
		&rpcCfg, chainntnfs.NetParams, hintCache, hintCache, blockCache,
	)
	if err != nil {
		t.Fatalf("unable to create notifier: %v", err)
	}
	if err := notifier.Start(); err != nil {
		t.Fatalf("unable to start notifier: %v", err)
	}

	return notifier
}

// TestHistoricalConfDetailsTxIndex ensures that we correctly retrieve
// historical confirmation details using the backend node's txindex.
func TestHistoricalConfDetailsTxIndex(t *testing.T) {
	t.Parallel()

	harness, tearDown := chainntnfs.NewMiner(
		t, []string{"--txindex"}, true, 25,
	)
	defer tearDown()

	notifier := setUpNotifier(t, harness)
	defer notifier.Stop()

	// A transaction unknown to the node should not be found within the
	// txindex even if it is enabled, so we should not proceed with any
	// fallback methods.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	unknownConfReq, err := chainntnfs.NewConfRequest(&unknownHash, testScript)
	if err != nil {
		t.Fatalf("unable to create conf request: %v", err)
	}
	_, txStatus, err := notifier.historicalConfDetails(unknownConfReq, 0, 0)
	if err != nil {
		t.Fatalf("unable to retrieve historical conf details: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to create tx: %v", err)
	}
	if err := chainntnfs.WaitForMempoolTx(harness, txid); err != nil {
		t.Fatalf("unable to find tx in the mempool: %v", err)
	}
	confReq, err := chainntnfs.NewConfRequest(txid, pkScript)
	if err != nil {
		t.Fatalf("unable to create conf request: %v", err)
	}

	// The transaction should be found in the mempool at this point.
	_, txStatus, err = notifier.historicalConfDetails(confReq, 0, 0)
	if err != nil {
		t.Fatalf("unable to retrieve historical conf details: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to retrieve historical conf details: %v", err)
	}

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

	harness, tearDown := chainntnfs.NewMiner(t, nil, true, 25)
	defer tearDown()

	notifier := setUpNotifier(t, harness)
	defer notifier.Stop()

	// Since the node has its txindex disabled, we fall back to scanning the
	// chain manually. A transaction unknown to the network should not be
	// found.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	unknownConfReq, err := chainntnfs.NewConfRequest(&unknownHash, testScript)
	if err != nil {
		t.Fatalf("unable to create conf request: %v", err)
	}
	_, txStatus, err := notifier.historicalConfDetails(unknownConfReq, 0, 0)
	if err != nil {
		t.Fatalf("unable to retrieve historical conf details: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to retrieve current height: %v", err)
	}

	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(harness)
	if err != nil {
		t.Fatalf("unable to create tx: %v", err)
	}
	if err := chainntnfs.WaitForMempoolTx(harness, txid); err != nil {
		t.Fatalf("unable to find tx in the mempool: %v", err)
	}
	confReq, err := chainntnfs.NewConfRequest(txid, pkScript)
	if err != nil {
		t.Fatalf("unable to create conf request: %v", err)
	}

	_, txStatus, err = notifier.historicalConfDetails(confReq, 0, 0)
	if err != nil {
		t.Fatalf("unable to retrieve historical conf details: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to retrieve historical conf details: %v", err)
	}

	// Since the backend node's txindex is disabled and the transaction has
	// confirmed, we should be able to find it by falling back to scanning
	// the chain manually.
	if txStatus != chainntnfs.TxFoundManually {
		t.Fatal("should have found the transaction by manually " +
			"scanning the chain, but did not")
	}
}
