package chainview

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb" // Required to register the boltdb walletdb implementation.
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/blockcache"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

var (
	netParams = &chaincfg.RegressionNetParams

	testPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	privKey, pubKey = btcec.PrivKeyFromBytes(testPrivKey)
	addrPk, _       = btcutil.NewAddressPubKey(pubKey.SerializeCompressed(),
		netParams)
	testAddr = addrPk.AddressPubKeyHash()

	testScript, _ = txscript.PayToAddrScript(testAddr)
)

func waitForMempoolTx(r *rpctest.Harness, txid *chainhash.Hash) error {
	var found bool
	var tx *btcutil.Tx
	var err error
	timeout := time.After(10 * time.Second)
	for !found {
		// Do a short wait
		select {
		case <-timeout:
			return fmt.Errorf("timeout after 10s")
		default:
		}
		time.Sleep(100 * time.Millisecond)

		// Check for the harness' knowledge of the txid
		tx, err = r.Client.GetRawTransaction(txid)
		if err != nil {
			switch e := err.(type) {
			case *btcjson.RPCError:
				if e.Code == btcjson.ErrRPCNoTxInfo {
					continue
				}
			default:
			}
			return err
		}
		if tx != nil && tx.MsgTx().TxHash() == *txid {
			found = true
		}
	}
	return nil
}

func getTestTXID(miner *rpctest.Harness) (*chainhash.Hash, error) {
	script, err := txscript.PayToAddrScript(testAddr)
	if err != nil {
		return nil, err
	}

	outputs := []*wire.TxOut{
		{
			Value:    2e8,
			PkScript: script,
		},
	}
	return miner.SendOutputs(outputs, 2500)
}

func locateOutput(tx *wire.MsgTx, script []byte) (*wire.OutPoint, *wire.TxOut, error) {
	for i, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, script) {
			return &wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: uint32(i),
			}, txOut, nil
		}
	}

	return nil, nil, fmt.Errorf("unable to find output")
}

func craftSpendTransaction(outpoint wire.OutPoint, payScript []byte) (*wire.MsgTx, error) {
	spendingTx := wire.NewMsgTx(1)
	spendingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outpoint,
	})
	spendingTx.AddTxOut(&wire.TxOut{
		Value:    1e8,
		PkScript: payScript,
	})
	sigScript, err := txscript.SignatureScript(spendingTx, 0, payScript,
		txscript.SigHashAll, privKey, true)
	if err != nil {
		return nil, err
	}
	spendingTx.TxIn[0].SignatureScript = sigScript

	return spendingTx, nil
}

func assertFilteredBlock(t *testing.T, fb *FilteredBlock, expectedHeight int32,
	expectedHash *chainhash.Hash, txns []*chainhash.Hash) {

	_, _, line, _ := runtime.Caller(1)

	if fb.Height != uint32(expectedHeight) {
		t.Fatalf("line %v: block height mismatch: expected %v, got %v",
			line, expectedHeight, fb.Height)
	}
	if !bytes.Equal(fb.Hash[:], expectedHash[:]) {
		t.Fatalf("line %v: block hash mismatch: expected %v, got %v",
			line, expectedHash, fb.Hash)
	}
	if len(fb.Transactions) != len(txns) {
		t.Fatalf("line %v: expected %v transaction in filtered block, instead "+
			"have %v", line, len(txns), len(fb.Transactions))
	}

	expectedTxids := make(map[chainhash.Hash]struct{})
	for _, txn := range txns {
		expectedTxids[*txn] = struct{}{}
	}

	for _, tx := range fb.Transactions {
		txid := tx.TxHash()
		delete(expectedTxids, txid)
	}

	if len(expectedTxids) != 0 {
		t.Fatalf("line %v: missing txids: %v", line, expectedTxids)
	}

}

// setupMinerAndChainView creates a miner node and initialize a chain view using
// this miner node. It returns the miner and the chain view.
func setupMinerAndChainView(t *testing.T, chainViewInit chainViewInitFunc) (
	*rpctest.Harness, FilteredChainView) {

	// Initialize the harness around a btcd node which will serve as our
	// dedicated miner to generate blocks, cause re-orgs, etc. We'll set up
	// this node with a chain length of 125, so we have plenty of BTC to
	// play around with.
	miner := unittest.NewMiner(
		t, netParams, []string{"--txindex"}, true, 25,
	)
	rpcConfig := miner.RPCConfig()

	_, bestHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err)

	chainView, err := chainViewInit(t, rpcConfig, miner, bestHeight)
	require.NoError(t, err)

	err = chainView.Start()
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, chainView.Stop())
	})

	return miner, chainView
}

func testFilterBlockNotifications(t *testing.T,
	chainViewInit chainViewInitFunc) {

	node, chainView := setupMinerAndChainView(t, chainViewInit)

	// To start the test, we'll create to fresh outputs paying to the
	// private key that we generated above.
	txid1, err := getTestTXID(node)
	require.NoError(t, err, "unable to get test txid")
	err = waitForMempoolTx(node, txid1)
	require.NoError(t, err, "unable to get test txid in mempool")
	txid2, err := getTestTXID(node)
	require.NoError(t, err, "unable to get test txid")
	err = waitForMempoolTx(node, txid2)
	require.NoError(t, err, "unable to get test txid in mempool")

	blockChan := chainView.FilteredBlocks()

	// Next we'll mine a block confirming the output generated above.
	newBlockHashes, err := node.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")

	_, currentHeight, err := node.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// We should get an update, however it shouldn't yet contain any
	// filtered transaction as the filter hasn't been update.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(
			t, filteredBlock, currentHeight,
			newBlockHashes[0], []*chainhash.Hash{},
		)
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	// Now that the block has been mined, we'll fetch the two transactions
	// so we can add them to the filter, and also craft transaction
	// spending the outputs we created.
	tx1, err := node.Client.GetRawTransaction(txid1)
	require.NoError(t, err, "unable to fetch transaction")
	tx2, err := node.Client.GetRawTransaction(txid2)
	require.NoError(t, err, "unable to fetch transaction")

	targetScript, err := txscript.PayToAddrScript(testAddr)
	require.NoError(t, err, "unable to create target output")

	// Next, we'll locate the two outputs generated above that pay to use
	// so we can properly add them to the filter.
	outPoint1, _, err := locateOutput(tx1.MsgTx(), targetScript)
	require.NoError(t, err, "unable to find output")
	outPoint2, _, err := locateOutput(tx2.MsgTx(), targetScript)
	require.NoError(t, err, "unable to find output")

	_, currentHeight, err = node.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Now we'll add both outpoints to the current filter.
	filter := []graphdb.EdgePoint{
		{FundingPkScript: targetScript, OutPoint: *outPoint1},
		{FundingPkScript: targetScript, OutPoint: *outPoint2},
	}
	err = chainView.UpdateFilter(filter, uint32(currentHeight))
	require.NoError(t, err, "unable to update filter")

	// With the filter updated, we'll now create two transaction spending
	// the outputs we created.
	spendingTx1, err := craftSpendTransaction(*outPoint1, targetScript)
	require.NoError(t, err, "unable to create spending tx")
	spendingTx2, err := craftSpendTransaction(*outPoint2, targetScript)
	require.NoError(t, err, "unable to create spending tx")

	// Now we'll broadcast the first spending transaction and also mine a
	// block which should include it.
	spendTxid1, err := node.Client.SendRawTransaction(spendingTx1, true)
	require.NoError(t, err, "unable to broadcast transaction")
	err = waitForMempoolTx(node, spendTxid1)
	require.NoError(t, err, "unable to get spending txid in mempool")
	newBlockHashes, err = node.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")

	// We should receive a notification over the channel. The notification
	// should correspond to the current block height and have that single
	// filtered transaction.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight+1,
			newBlockHashes[0], []*chainhash.Hash{spendTxid1})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	// Next, mine the second transaction which spends the second output.
	// This should also generate a notification.
	spendTxid2, err := node.Client.SendRawTransaction(spendingTx2, true)
	require.NoError(t, err, "unable to broadcast transaction")
	err = waitForMempoolTx(node, spendTxid2)
	require.NoError(t, err, "unable to get spending txid in mempool")
	newBlockHashes, err = node.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")

	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight+2,
			newBlockHashes[0], []*chainhash.Hash{spendTxid2})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}
}

func testUpdateFilterBackTrack(t *testing.T, chainViewInit chainViewInitFunc) {
	node, chainView := setupMinerAndChainView(t, chainViewInit)

	// To start, we'll create a fresh output paying to the height generated
	// above.
	txid, err := getTestTXID(node)
	if err != nil {
		t.Fatalf("unable to get test txid")
	}
	err = waitForMempoolTx(node, txid)
	require.NoError(t, err, "unable to get test txid in mempool")

	// Next we'll mine a block confirming the output generated above.
	initBlockHashes, err := node.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")

	blockChan := chainView.FilteredBlocks()

	_, currentHeight, err := node.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Consume the notification sent which contains an empty filtered
	// block.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight,
			initBlockHashes[0], []*chainhash.Hash{})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	// Next, create a transaction which spends the output created above,
	// mining the spend into a block.
	tx, err := node.Client.GetRawTransaction(txid)
	require.NoError(t, err, "unable to fetch transaction")
	outPoint, _, err := locateOutput(tx.MsgTx(), testScript)
	require.NoError(t, err, "unable to find output")
	spendingTx, err := craftSpendTransaction(*outPoint, testScript)
	require.NoError(t, err, "unable to create spending tx")
	spendTxid, err := node.Client.SendRawTransaction(spendingTx, true)
	require.NoError(t, err, "unable to broadcast transaction")
	err = waitForMempoolTx(node, spendTxid)
	require.NoError(t, err, "unable to get spending txid in mempool")
	newBlockHashes, err := node.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")

	// We should have received another empty filtered block notification.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight+1,
			newBlockHashes[0], []*chainhash.Hash{})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	// After the block has been mined+notified we'll update the filter with
	// a _prior_ height so a "rewind" occurs.
	filter := []graphdb.EdgePoint{
		{FundingPkScript: testScript, OutPoint: *outPoint},
	}
	err = chainView.UpdateFilter(filter, uint32(currentHeight))
	require.NoError(t, err, "unable to update filter")

	// We should now receive a fresh filtered block notification that
	// includes the transaction spend we included above.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight+1,
			newBlockHashes[0], []*chainhash.Hash{spendTxid})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}
}

func testFilterSingleBlock(t *testing.T, chainViewInit chainViewInitFunc) {
	node, chainView := setupMinerAndChainView(t, chainViewInit)

	// In this test, we'll test the manual filtration of blocks, which can
	// be used by clients to manually rescan their sub-set of the UTXO set.

	// First, we'll create a block that includes two outputs that we're
	// able to spend with the private key generated above.
	txid1, err := getTestTXID(node)
	if err != nil {
		t.Fatalf("unable to get test txid")
	}
	err = waitForMempoolTx(node, txid1)
	require.NoError(t, err, "unable to get test txid in mempool")
	txid2, err := getTestTXID(node)
	if err != nil {
		t.Fatalf("unable to get test txid")
	}
	err = waitForMempoolTx(node, txid2)
	require.NoError(t, err, "unable to get test txid in mempool")

	blockChan := chainView.FilteredBlocks()

	// Next we'll mine a block confirming the output generated above.
	newBlockHashes, err := node.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")

	_, currentHeight, err := node.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// We should get an update, however it shouldn't yet contain any
	// filtered transaction as the filter hasn't been updated.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight,
			newBlockHashes[0], []*chainhash.Hash{})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	tx1, err := node.Client.GetRawTransaction(txid1)
	require.NoError(t, err, "unable to fetch transaction")
	tx2, err := node.Client.GetRawTransaction(txid2)
	require.NoError(t, err, "unable to fetch transaction")

	// Next, we'll create a block that includes two transactions, each
	// which spend one of the outputs created.
	outPoint1, _, err := locateOutput(tx1.MsgTx(), testScript)
	require.NoError(t, err, "unable to find output")
	outPoint2, _, err := locateOutput(tx2.MsgTx(), testScript)
	require.NoError(t, err, "unable to find output")
	spendingTx1, err := craftSpendTransaction(*outPoint1, testScript)
	require.NoError(t, err, "unable to create spending tx")
	spendingTx2, err := craftSpendTransaction(*outPoint2, testScript)
	require.NoError(t, err, "unable to create spending tx")
	txns := []*btcutil.Tx{btcutil.NewTx(spendingTx1), btcutil.NewTx(spendingTx2)}
	block, err := node.GenerateAndSubmitBlock(txns, 11, time.Time{})
	require.NoError(t, err, "unable to generate block")

	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight+1,
			block.Hash(), []*chainhash.Hash{})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	_, currentHeight, err = node.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Now we'll manually trigger filtering the block generated above.
	// First, we'll add the two outpoints to our filter.
	filter := []graphdb.EdgePoint{
		{FundingPkScript: testScript, OutPoint: *outPoint1},
		{FundingPkScript: testScript, OutPoint: *outPoint2},
	}
	err = chainView.UpdateFilter(filter, uint32(currentHeight))
	require.NoError(t, err, "unable to update filter")

	// We set the filter with the current height, so we shouldn't get any
	// notifications.
	select {
	case <-blockChan:
		t.Fatalf("got filter notification, but shouldn't have")
	default:
	}

	// Now we'll manually rescan that past block. This should include two
	// filtered transactions, the spending transactions we created above.
	filteredBlock, err := chainView.FilterBlock(block.Hash())
	require.NoError(t, err, "unable to filter block")
	txn1, txn2 := spendingTx1.TxHash(), spendingTx2.TxHash()
	expectedTxns := []*chainhash.Hash{&txn1, &txn2}
	assertFilteredBlock(t, filteredBlock, currentHeight, block.Hash(),
		expectedTxns)
}

// testFilterBlockDisconnected triggers a reorg all the way back to genesis,
// and a small 5 block reorg, ensuring the chainView notifies about
// disconnected and connected blocks in the order we expect.
func testFilterBlockDisconnected(t *testing.T,
	chainViewInit chainViewInitFunc) {

	node, _ := setupMinerAndChainView(t, chainViewInit)

	// Create a node that has a shorter chain than the main chain, so we
	// can trigger a reorg.
	reorgNode := unittest.NewMiner(
		t, netParams, []string{"--txindex"}, true, 5,
	)

	_, bestHeight, err := reorgNode.Client.GetBestBlock()
	require.NoError(t, err, "error getting best block")

	// Init a chain view that has this node as its block source.
	reorgView, err := chainViewInit(
		t, reorgNode.RPCConfig(), reorgNode, bestHeight,
	)
	require.NoError(t, err, "unable to create chain view")

	if err = reorgView.Start(); err != nil {
		t.Fatalf("unable to start btcd chain view: %v", err)
	}
	defer reorgView.Stop()

	newBlocks := reorgView.FilteredBlocks()
	disconnectedBlocks := reorgView.DisconnectedBlocks()

	// If this the neutrino backend, then we'll give it some time to catch
	// up, as it's a bit slower to consume new blocks compared to the RPC
	// backends.
	if _, ok := reorgView.(*CfFilteredChainView); ok {
		time.Sleep(time.Second * 3)
	}

	_, oldHeight, err := reorgNode.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Now connect the node with the short chain to the main node, and wait
	// for their chains to synchronize. The short chain will be reorged all
	// the way back to genesis.
	if err := rpctest.ConnectNode(reorgNode, node); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice := []*rpctest.Harness{node, reorgNode}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	_, newHeight, err := reorgNode.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// We should be getting oldHeight number of blocks marked as
	// stale/disconnected. We expect to first get all stale blocks,
	// then the new blocks. We also ensure a strict ordering.
	for i := int32(0); i < oldHeight+newHeight; i++ {
		select {
		case block := <-newBlocks:
			if i < oldHeight {
				t.Fatalf("did not expect to get new block "+
					"in iteration %d, old height: %v", i,
					oldHeight)
			}
			expectedHeight := uint32(i - oldHeight + 1)
			if block.Height != expectedHeight {
				t.Fatalf("expected to receive connected "+
					"block at height %d, instead got at %d",
					expectedHeight, block.Height)
			}
		case block := <-disconnectedBlocks:
			if i >= oldHeight {
				t.Fatalf("did not expect to get stale block "+
					"in iteration %d", i)
			}
			expectedHeight := uint32(oldHeight - i)
			if block.Height != expectedHeight {
				t.Fatalf("expected to receive disconnected "+
					"block at height %d, instead got at %d",
					expectedHeight, block.Height)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout waiting for block")
		}
	}

	// Now we trigger a small reorg, by disconnecting the nodes, mining
	// a few blocks on each, then connecting them again.
	peers, err := reorgNode.Client.GetPeerInfo()
	require.NoError(t, err, "unable to get peer info")
	numPeers := len(peers)

	// Disconnect the nodes.
	err = reorgNode.Client.AddNode(node.P2PAddress(), rpcclient.ANRemove)
	require.NoError(t, err, "unable to disconnect mining nodes")

	// Wait for disconnection
	for {
		peers, err = reorgNode.Client.GetPeerInfo()
		if err != nil {
			t.Fatalf("unable to get peer info: %v", err)
		}
		if len(peers) < numPeers {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Mine 10 blocks on the main chain, 5 on the chain that will be
	// reorged out,
	if _, err := node.Client.Generate(10); err != nil {
		t.Fatalf("unable to generate blocks on main chain: %v", err)
	}
	if _, err := reorgNode.Client.Generate(5); err != nil {
		t.Fatalf("unable to generate blocks on reorged chain: %v", err)
	}

	// 5 new blocks should get notified.
	for i := uint32(0); i < 5; i++ {
		select {
		case block := <-newBlocks:
			expectedHeight := uint32(newHeight) + i + 1
			if block.Height != expectedHeight {
				t.Fatalf("expected to receive connected "+
					"block at height %d, instead got at %d",
					expectedHeight, block.Height)
			}
		case <-disconnectedBlocks:
			t.Fatalf("did not expect to get stale block "+
				"in iteration %d", i)
		case <-time.After(10 * time.Second):
			t.Fatalf("did not get connected block")
		}
	}

	_, oldHeight, err = reorgNode.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Now connect the two nodes, and wait for their chains to sync up.
	if err := rpctest.ConnectNode(reorgNode, node); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	if _, _, err := reorgNode.Client.GetBestBlock(); err != nil {
		t.Fatalf("unable to get current height: %v", err)
	}

	// We should get 5 disconnected, 10 connected blocks.
	for i := uint32(0); i < 15; i++ {
		select {
		case block := <-newBlocks:
			if i < 5 {
				t.Fatalf("did not expect to get new block "+
					"in iteration %d", i)
			}
			// The expected height for the connected block will be
			// oldHeight - 5 (the 5 disconnected blocks) + (i-5)
			// (subtract 5 since the 5 first iterations consumed
			// disconnected blocks) + 1
			expectedHeight := uint32(oldHeight) - 9 + i
			if block.Height != expectedHeight {
				t.Fatalf("expected to receive connected "+
					"block at height %d, instead got at %d",
					expectedHeight, block.Height)
			}
		case block := <-disconnectedBlocks:
			if i >= 5 {
				t.Fatalf("did not expect to get stale block "+
					"in iteration %d", i)
			}
			expectedHeight := uint32(oldHeight) - i
			if block.Height != expectedHeight {
				t.Fatalf("expected to receive disconnected "+
					"block at height %d, instead got at %d",
					expectedHeight, block.Height)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("did not get disconnected block")
		}
	}

	// Time for db access to finish between testcases.
	time.Sleep(time.Millisecond * 500)
}

type chainViewInitFunc func(t *testing.T, rpcInfo rpcclient.ConnConfig,
	miner *rpctest.Harness, bestHeight int32) (FilteredChainView, error)

type testCase struct {
	name string
	test func(*testing.T, chainViewInitFunc)
}

var chainViewTests = []testCase{
	{
		name: "filtered block ntfns",
		test: testFilterBlockNotifications,
	},
	{
		name: "update filter back track",
		test: testUpdateFilterBackTrack,
	},
	{
		name: "filter single block",
		test: testFilterSingleBlock,
	},
	{
		name: "filter block disconnected",
		test: testFilterBlockDisconnected,
	},
}

var interfaceImpls = []struct {
	name          string
	chainViewInit chainViewInitFunc
}{
	{
		name: "bitcoind_zmq",
		chainViewInit: func(t *testing.T, _ rpcclient.ConnConfig,
			miner *rpctest.Harness, bestHeight int32) (
			FilteredChainView, error) {

			// Start a bitcoind instance.
			chainConn := unittest.NewBitcoindBackend(
				t, unittest.NetParams, miner, true,
				false,
			)
			blockCache := blockcache.NewBlockCache(10000)

			chainView := NewBitcoindFilteredChainView(
				chainConn, blockCache,
			)

			return chainView, nil
		},
	},
	{
		name: "bitcoind_polling",
		chainViewInit: func(t *testing.T, _ rpcclient.ConnConfig,
			miner *rpctest.Harness, bestHeight int32) (
			FilteredChainView, error) {

			// Wait for the bitcoind instance to start up.
			chainConn := unittest.NewBitcoindBackend(
				t, unittest.NetParams, miner, true,
				true,
			)
			blockCache := blockcache.NewBlockCache(10000)

			chainView := NewBitcoindFilteredChainView(
				chainConn, blockCache,
			)

			// When running in rpc polling mode, the `reorg` method
			// in `BitcoindClient`'s `ntfnHandler` may be invoked to
			// handle the last block received from the miner during
			// bitcoind's startup. This behavior will cause a block
			// disconnected and a block connected notifications to
			// be sent to the channels.
			//
			// TODO(yy): unify the chain backend logic and put
			// everything in `btcwallet/chain` instead. The only
			// place we use this chain view is in `graph/builder`,
			// in which we subscribe to `FilteredBlocks` and
			// `DisconnectedBlocks`.
			time.Sleep(1 * time.Second)

			return chainView, nil
		},
	},
	{
		name: "p2p_neutrino",
		chainViewInit: func(t *testing.T, _ rpcclient.ConnConfig,
			miner *rpctest.Harness, bestHeight int32) (
			FilteredChainView, error) {

			spvDir := t.TempDir()

			dbName := filepath.Join(spvDir, "neutrino.db")
			spvDatabase, err := walletdb.Create(
				kvdb.BoltBackendName, dbName, true,
				kvdb.DefaultDBTimeout, false,
			)
			if err != nil {
				return nil, err
			}

			spvConfig := neutrino.Config{
				DataDir:      spvDir,
				Database:     spvDatabase,
				ChainParams:  *netParams,
				ConnectPeers: []string{miner.P2PAddress()},
			}

			spvNode, err := neutrino.NewChainService(spvConfig)
			if err != nil {
				return nil, err
			}

			// Wait until the node has fully synced up to the local
			// btcd node.
			err = wait.NoError(func() error {
				err := spvNode.Start()
				if err != nil {
					return err
				}

				bestBlock, err := spvNode.BestBlock()
				if err != nil {
					return err
				}

				if bestBlock.Height < bestHeight {
					return fmt.Errorf("not synced yet")
				}

				return nil
			}, 10*time.Second)
			if err != nil {
				return nil, fmt.Errorf("unable to "+
					"establish connection to bitcoind: %v",
					err)
			}

			t.Cleanup(func() {
				spvDatabase.Close()
				spvNode.Stop()
			})

			blockCache := blockcache.NewBlockCache(10000)

			chainView, err := NewCfFilteredChainView(
				spvNode, blockCache,
			)
			if err != nil {
				return nil, err
			}

			return chainView, nil
		},
	},
	{
		name: "btcd_websockets",
		chainViewInit: func(_ *testing.T, config rpcclient.ConnConfig,
			_ *rpctest.Harness, _ int32) (
			FilteredChainView, error) {

			blockCache := blockcache.NewBlockCache(10000)
			chainView, err := NewBtcdFilteredChainView(
				config, blockCache,
			)
			if err != nil {
				return nil, err
			}

			return chainView, err
		},
	},
}

func TestFilteredChainView(t *testing.T) {
	for _, chainViewImpl := range interfaceImpls {
		t.Logf("Testing '%v' implementation of FilteredChainView",
			chainViewImpl.name)

		for _, chainViewTest := range chainViewTests {
			testName := fmt.Sprintf("%v: %v", chainViewImpl.name,
				chainViewTest.name)

			success := t.Run(testName, func(t *testing.T) {
				chainViewTest.test(
					t, chainViewImpl.chainViewInit,
				)
			})

			if !success {
				break
			}
		}

	}
}
