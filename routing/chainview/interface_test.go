package chainview

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/lightninglabs/neutrino"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/integration/rpctest"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/walletdb"

	_ "github.com/roasbeef/btcwallet/walletdb/bdb" // Required to register the boltdb walletdb implementation.
)

var (
	netParams = &chaincfg.SimNetParams

	testPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), testPrivKey)
	addrPk, _       = btcutil.NewAddressPubKey(pubKey.SerializeCompressed(),
		netParams)
	testAddr = addrPk.AddressPubKeyHash()

	testScript, _ = txscript.PayToAddrScript(testAddr)
)

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
	return miner.SendOutputs(outputs, 10)
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

func testFilterBlockNotifications(node *rpctest.Harness,
	chainView FilteredChainView, t *testing.T) {

	// To start the test, we'll create to fresh outputs paying to the
	// private key that we generated above.
	txid1, err := getTestTXID(node)
	if err != nil {
		t.Fatalf("unable to get test txid")
	}
	txid2, err := getTestTXID(node)
	if err != nil {
		t.Fatalf("unable to get test txid")
	}

	blockChan := chainView.FilteredBlocks()

	// Next we'll mine a block confirming the output generated above.
	newBlockHashes, err := node.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	_, currentHeight, err := node.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current height: %v", err)
	}

	// We should get an update, however it shouldn't yet contain any
	// filtered transaction as the filter hasn't been update.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight,
			newBlockHashes[0], []*chainhash.Hash{})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	// Now that the block has been mined, we'll fetch the two transactions
	// so we can add them to the filter, and also craft transaction
	// spending the outputs we created.
	tx1, err := node.Node.GetRawTransaction(txid1)
	if err != nil {
		t.Fatalf("unable to fetch transaction: %v", err)
	}
	tx2, err := node.Node.GetRawTransaction(txid2)
	if err != nil {
		t.Fatalf("unable to fetch transaction: %v", err)
	}

	targetScript, err := txscript.PayToAddrScript(testAddr)
	if err != nil {
		t.Fatalf("unable to create target output: %v", err)
	}

	// Next, we'll locate the two outputs generated above that pay to use
	// so we can properly add them to the filter.
	outPoint1, _, err := locateOutput(tx1.MsgTx(), targetScript)
	if err != nil {
		t.Fatalf("unable to find output: %v", err)
	}
	outPoint2, _, err := locateOutput(tx2.MsgTx(), targetScript)
	if err != nil {
		t.Fatalf("unable to find output: %v", err)
	}

	_, currentHeight, err = node.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current height: %v", err)
	}

	// Now we'll add both output to the current filter.
	filter := []wire.OutPoint{*outPoint1, *outPoint2}
	err = chainView.UpdateFilter(filter, uint32(currentHeight))
	if err != nil {
		t.Fatalf("unable to update filter: %v", err)
	}

	// With the filter updated, we'll now create two transaction spending
	// the outputs we created.
	spendingTx1, err := craftSpendTransaction(*outPoint1, targetScript)
	if err != nil {
		t.Fatalf("unable to create spending tx: %v", err)
	}
	spendingTx2, err := craftSpendTransaction(*outPoint2, targetScript)
	if err != nil {
		t.Fatalf("unable to create spending tx: %v", err)
	}

	// Now we'll broadcast the first spending transaction and also mine a
	// block which should include it.
	spendTxid1, err := node.Node.SendRawTransaction(spendingTx1, true)
	if err != nil {
		t.Fatalf("unable to broadcast transaction: %v", err)
	}
	newBlockHashes, err = node.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

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
	spendTxid2, err := node.Node.SendRawTransaction(spendingTx2, true)
	if err != nil {
		t.Fatalf("unable to broadcast transaction: %v", err)
	}
	newBlockHashes, err = node.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight+2,
			newBlockHashes[0], []*chainhash.Hash{spendTxid2})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}
}

func testUpdateFilterBackTrack(node *rpctest.Harness, chainView FilteredChainView,
	t *testing.T) {

	// To start, we'll create a fresh output paying to the height generated
	// above.
	txid, err := getTestTXID(node)
	if err != nil {
		t.Fatalf("unable to get test txid")
	}

	// Next we'll mine a block confirming the output generated above.
	initBlockHashes, err := node.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	blockChan := chainView.FilteredBlocks()

	_, currentHeight, err := node.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current height: %v", err)
	}

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
	tx, err := node.Node.GetRawTransaction(txid)
	if err != nil {
		t.Fatalf("unable to fetch transaction: %v", err)
	}
	outPoint, _, err := locateOutput(tx.MsgTx(), testScript)
	if err != nil {
		t.Fatalf("unable to find output: %v", err)
	}
	spendingTx, err := craftSpendTransaction(*outPoint, testScript)
	if err != nil {
		t.Fatalf("unable to create spending tx: %v", err)
	}
	spendTxid, err := node.Node.SendRawTransaction(spendingTx, true)
	if err != nil {
		t.Fatalf("unable to broadcast transaction: %v", err)
	}
	newBlockHashes, err := node.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// We should've received another empty filtered block notification.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight+1,
			newBlockHashes[0], []*chainhash.Hash{})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	// After the block has been mined+notified we'll update the filter with
	// a _prior_ height so a "rewind" occurs.
	filter := []wire.OutPoint{*outPoint}
	err = chainView.UpdateFilter(filter, uint32(currentHeight))
	if err != nil {
		t.Fatalf("unable to update filter: %v", err)
	}

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

func testFilterSingleBlock(node *rpctest.Harness, chainView FilteredChainView,
	t *testing.T) {

	// In this test, we'll test the manual filtration of blocks, which can
	// be used by clients to manually rescan their sub-set of the UTXO set.

	// First, we'll create a block that includes two outputs that we're
	// able to spend with the private key generated above.
	txid1, err := getTestTXID(node)
	if err != nil {
		t.Fatalf("unable to get test txid")
	}
	txid2, err := getTestTXID(node)
	if err != nil {
		t.Fatalf("unable to get test txid")
	}

	blockChan := chainView.FilteredBlocks()

	// Next we'll mine a block confirming the output generated above.
	newBlockHashes, err := node.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	_, currentHeight, err := node.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current height: %v", err)
	}

	// We should get an update, however it shouldn't yet contain any
	// filtered transaction as the filter hasn't been updated.
	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight,
			newBlockHashes[0], []*chainhash.Hash{})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	tx1, err := node.Node.GetRawTransaction(txid1)
	if err != nil {
		t.Fatalf("unable to fetch transaction: %v", err)
	}
	tx2, err := node.Node.GetRawTransaction(txid2)
	if err != nil {
		t.Fatalf("unable to fetch transaction: %v", err)
	}

	// Next, we'll create a block that includes two transactions, each
	// which spend one of the outputs created.
	outPoint1, _, err := locateOutput(tx1.MsgTx(), testScript)
	if err != nil {
		t.Fatalf("unable to find output: %v", err)
	}
	outPoint2, _, err := locateOutput(tx2.MsgTx(), testScript)
	if err != nil {
		t.Fatalf("unable to find output: %v", err)
	}
	spendingTx1, err := craftSpendTransaction(*outPoint1, testScript)
	if err != nil {
		t.Fatalf("unable to create spending tx: %v", err)
	}
	spendingTx2, err := craftSpendTransaction(*outPoint2, testScript)
	if err != nil {
		t.Fatalf("unable to create spending tx: %v", err)
	}
	txns := []*btcutil.Tx{btcutil.NewTx(spendingTx1), btcutil.NewTx(spendingTx2)}
	block, err := node.GenerateAndSubmitBlock(txns, 11, time.Time{})
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	select {
	case filteredBlock := <-blockChan:
		assertFilteredBlock(t, filteredBlock, currentHeight+1,
			block.Hash(), []*chainhash.Hash{})
	case <-time.After(time.Second * 20):
		t.Fatalf("filtered block notification didn't arrive")
	}

	_, currentHeight, err = node.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current height: %v", err)
	}

	// Now we'll manually trigger filtering the block generated above.
	// First, we'll add the two outpoints to our filter.
	filter := []wire.OutPoint{*outPoint1, *outPoint2}
	err = chainView.UpdateFilter(filter, uint32(currentHeight))
	if err != nil {
		t.Fatalf("unable to update filter: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to filter block: %v", err)
	}
	txn1, txn2 := spendingTx1.TxHash(), spendingTx2.TxHash()
	expectedTxns := []*chainhash.Hash{&txn1, &txn2}
	assertFilteredBlock(t, filteredBlock, currentHeight, block.Hash(),
		expectedTxns)
}

type testCase struct {
	name string
	test func(*rpctest.Harness, FilteredChainView, *testing.T)
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
		name: "fitler single block",
		test: testFilterSingleBlock,
	},
}

var interfaceImpls = []struct {
	name          string
	chainViewInit func(rpcInfo rpcclient.ConnConfig,
		p2pAddr string) (func(), FilteredChainView, error)
}{
	{
		name: "p2p_neutrino",
		chainViewInit: func(_ rpcclient.ConnConfig, p2pAddr string) (func(), FilteredChainView, error) {
			spvDir, err := ioutil.TempDir("", "neutrino")
			if err != nil {
				return nil, nil, err
			}

			dbName := filepath.Join(spvDir, "neutrino.db")
			spvDatabase, err := walletdb.Create("bdb", dbName)
			if err != nil {
				return nil, nil, err
			}

			spvConfig := neutrino.Config{
				DataDir:      spvDir,
				Database:     spvDatabase,
				ChainParams:  *netParams,
				ConnectPeers: []string{p2pAddr},
			}

			neutrino.WaitForMoreCFHeaders = 250 * time.Millisecond
			spvNode, err := neutrino.NewChainService(spvConfig)
			if err != nil {
				return nil, nil, err
			}
			spvNode.Start()

			// Wait until the node has fully synced up to the local
			// btcd node.
			for !spvNode.IsCurrent() {
				time.Sleep(time.Millisecond * 100)
			}

			cleanUp := func() {
				spvDatabase.Close()
				spvNode.Stop()
				os.RemoveAll(spvDir)
			}

			chainView, err := NewCfFilteredChainView(spvNode)
			if err != nil {
				return nil, nil, err
			}

			return cleanUp, chainView, nil
		},
	},
	{
		name: "btcd_websockets",
		chainViewInit: func(config rpcclient.ConnConfig, _ string) (func(), FilteredChainView, error) {
			chainView, err := NewBtcdFilteredChainView(config)
			if err != nil {
				return nil, nil, err
			}

			return nil, chainView, err
		},
	},
}

func TestFilteredChainView(t *testing.T) {
	// Initialize the harness around a btcd node which will serve as our
	// dedicated miner to generate blocks, cause re-orgs, etc. We'll set up
	// this node with a chain length of 125, so we have plentyyy of BTC to
	// play around with.
	miner, err := rpctest.New(netParams, nil, nil)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	defer miner.TearDown()
	if err := miner.SetUp(true, 25); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}

	rpcConfig := miner.RPCConfig()
	p2pAddr := miner.P2PAddress()

	for _, chainViewImpl := range interfaceImpls {
		t.Logf("Testing '%v' implementation of FilteredChainView",
			chainViewImpl.name)

		cleanUpFunc, chainView, err := chainViewImpl.chainViewInit(rpcConfig, p2pAddr)
		if err != nil {
			t.Fatalf("unable to make chain view: %v", err)
		}

		if err := chainView.Start(); err != nil {
			t.Fatalf("unable to start chain view: %v", err)
		}
		for _, chainViewTest := range chainViewTests {
			testName := fmt.Sprintf("%v: %v", chainViewImpl.name,
				chainViewTest.name)

			success := t.Run(testName, func(t *testing.T) {
				chainViewTest.test(miner, chainView, t)
			})

			if !success {
				break
			}
		}

		if err := chainView.Stop(); err != nil {
			t.Fatalf("unable to stop chain view: %v", err)
		}

		if cleanUpFunc != nil {
			cleanUpFunc()
		}
	}
}
