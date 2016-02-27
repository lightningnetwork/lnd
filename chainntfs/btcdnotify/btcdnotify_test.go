package btcdnotify

import (
	"bytes"
	"testing"
	"time"

	"github.com/Roasbeef/btcd/rpctest"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntfs"
)

var (
	testPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	netParams       = &chaincfg.SimNetParams
	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), testPrivKey)
	addrPk, _       = btcutil.NewAddressPubKey(pubKey.SerializeCompressed(),
		netParams)
	testAddr = addrPk.AddressPubKeyHash()
)

func getTestTxId(miner *rpctest.Harness) (*wire.ShaHash, error) {
	// First, parse the test priv key in order to obtain a key we'll use
	// for the confirmation notification test.
	outputMap := map[string]btcutil.Amount{testAddr.String(): 2e8}
	return miner.CoinbaseSpend(outputMap)
}
func testSingleConfirmationNotification(miner *rpctest.Harness,
	notifier chainntnfs.ChainNotifier, t *testing.T) {

	// We'd like to test the case of being notified once a txid reaches
	// a *single* confirmation.
	//
	// So first, let's send some coins to "ourself", obtainig a txid.
	// We're spending from a coinbase output here, so we use the dedicated
	// function.

	txid, err := getTestTxId(miner)
	if err != nil {
		t.Fatalf("unable to create test addr: %v", err)
	}

	// Now that we have a txid, register a confirmation notiication with
	// the chainntfn source.
	numConfs := uint32(1)
	confIntent, err := notifier.RegisterConfirmationsNtfn(txid, numConfs)
	if err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Now generate a single block, the transaction should be included which
	// should trigger a notification event.
	if _, err := miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	confSent := make(chan struct{})
	go func() {
		confSent <- <-confIntent.Confirmed
	}()

	select {
	case <-confSent:
		break
	case <-time.After(2 * time.Second):
		t.Fatalf("confirmation notification never received")
	}
}

func testMultiConfirmationNotification(miner *rpctest.Harness,
	notifier chainntnfs.ChainNotifier, t *testing.T) {

	// We'd like to test the case of being notified once a txid reaches
	// N confirmations, where N > 1.
	//
	// Again, we'll begin by creating a fresh transaction, so we can obtain a fresh txid.
	txid, err := getTestTxId(miner)
	if err != nil {
		t.Fatalf("unable to create test addr: %v", err)
	}

	numConfs := uint32(6)
	confIntent, err := notifier.RegisterConfirmationsNtfn(txid, numConfs)
	if err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Now generate a six blocks. The transaction should be included in the
	// first block, which will be built upon by the other 5 blocks.
	if _, err := miner.Node.Generate(6); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	confSent := make(chan struct{})
	go func() {
		confSent <- <-confIntent.Confirmed
	}()

	select {
	case <-confSent:
		break
	case <-time.After(2 * time.Second):
		t.Fatalf("confirmation notification never received")
	}
}

func testBatchConfirmationNotification(miner *rpctest.Harness,
	notifier chainntnfs.ChainNotifier, t *testing.T) {

	// We'd like to test a case of serving notifiations to multiple
	// clients, each requesting to be notified once a txid receives
	// various numbers of confirmations.
	confSpread := [6]uint32{1, 2, 3, 6, 20, 22}
	confIntents := make([]*chainntnfs.ConfirmationEvent, len(confSpread))

	// Create a new txid spending miner coins for each confirmation entry
	// in confSpread, we collect each conf intent into a slice so we can
	// verify they're each notified at the proper number of confirmations
	// below.
	for i, numConfs := range confSpread {
		txid, err := getTestTxId(miner)
		if err != nil {
			t.Fatalf("unable to create test addr: %v", err)
		}
		confIntent, err := notifier.RegisterConfirmationsNtfn(txid, numConfs)
		if err != nil {
			t.Fatalf("unable to register ntfn: %v", err)
		}
		confIntents[i] = confIntent
	}

	// Now, for each confirmation intent, generate the delta number of blocks
	// needed to trigger the confirmation notification. A goroutine is
	// spawned in order to verify the proper notification is triggered.
	for i, numConfs := range confSpread {
		var blocksToGen uint32

		// If this is the last instance, manually index to generate the
		// proper block delta in order to avoid a panic.
		if i == len(confSpread)-1 {
			blocksToGen = confSpread[len(confSpread)-1] - confSpread[len(confSpread)-2]
		} else {
			blocksToGen = confSpread[i+1] - confSpread[i]
		}

		// Generate the number of blocks necessary to trigger this
		// current confirmation notification.
		if _, err := miner.Node.Generate(blocksToGen); err != nil {
			t.Fatalf("unable to generate single block: %v", err)
		}

		confSent := make(chan struct{})
		go func() {
			confSent <- <-confIntents[i].Confirmed
		}()

		select {
		case <-confSent:
			continue
		case <-time.After(2 * time.Second):
			t.Fatalf("confirmation notification never received: %v", numConfs)
		}
	}
}

func testSpendNotification(miner *rpctest.Harness,
	notifier chainntnfs.ChainNotifier, t *testing.T) {

	// We'd like to test the spend notifiations for all
	// chainntnfs.ChainNotifier concrete implemenations.
	//
	// To do so, we first create a new output to our test target
	// address.
	txid, err := getTestTxId(miner)
	if err != nil {
		t.Fatalf("unable to create test addr: %v", err)
	}

	// Mine a single block which should include that txid above.
	if _, err := miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	// Now that we have the txid, fetch the transaction itself.
	wrappedTx, err := miner.Node.GetRawTransaction(txid)
	if err != nil {
		t.Fatalf("unable to get new tx: %v", err)
	}
	tx := wrappedTx.MsgTx()

	// Locate the output index sent to us. We need this so we can
	// construct a spending txn below.
	outIndex := -1
	var pkScript []byte
	for i, txOut := range tx.TxOut {
		if bytes.Contains(txOut.PkScript, testAddr.ScriptAddress()) {
			pkScript = txOut.PkScript
			outIndex = i
			break
		}
	}
	if outIndex == -1 {
		t.Fatalf("unable to locate new output")
	}

	// Now that we've found the output index, register for a spentness
	// notification for the newly created output.
	outpoint := wire.NewOutPoint(txid, uint32(outIndex))
	spentIntent, err := notifier.RegisterSpendNtfn(outpoint)
	if err != nil {
		t.Fatalf("unable to register for spend ntfn: %v", err)
	}

	// Next, create a new transaction spending that output.
	spendingTx := wire.NewMsgTx()
	spendingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *outpoint,
	})
	spendingTx.AddTxOut(&wire.TxOut{
		Value:    1e8,
		PkScript: pkScript,
	})
	sigScript, err := txscript.SignatureScript(spendingTx, 0, pkScript,
		txscript.SigHashAll, privKey, true)
	if err != nil {
		t.Fatalf("unable to sign tx: %v", err)
	}
	spendingTx.TxIn[0].SignatureScript = sigScript

	// Broadcast our spending transaction.
	spenderSha, err := miner.Node.SendRawTransaction(spendingTx, true)
	if err != nil {
		t.Fatalf("unable to brodacst tx: %v", err)
	}

	// Now we mine a single block, which should include our spend. The
	// notification should also be sent off.
	if _, err := miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	spentNtfn := make(chan *chainntnfs.SpendDetail)
	go func() {
		spentNtfn <- <-spentIntent.Spend
	}()

	select {
	case ntfn := <-spentNtfn:
		// We've received the spend nftn. So now verify all the fields
		// have been set properly.
		if ntfn.SpentOutPoint != outpoint {
			t.Fatalf("ntfn includes wrong output, reports %v instead of %v",
				ntfn.SpentOutPoint, outpoint)
		}
		if !bytes.Equal(ntfn.SpenderTxHash.Bytes(), spenderSha.Bytes()) {
			t.Fatalf("ntfn includes wrong spender tx sha, reports %v intead of %v",
				ntfn.SpenderTxHash.Bytes(), spenderSha.Bytes())
		}
		if ntfn.SpenderInputIndex != 0 {
			t.Fatalf("ntfn includes wrong spending input index, reports %v, should be %v",
				ntfn.SpenderInputIndex, 0)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("spend ntfn never received")
	}
}

var ntfnTests = []func(node *rpctest.Harness, notifier chainntnfs.ChainNotifier, t *testing.T){
	testSingleConfirmationNotification,
	testMultiConfirmationNotification,
	testBatchConfirmationNotification,
	testSpendNotification,
}

// TODO(roasbeef): make test generic across all interfaces?
//  * indeed!
//  * requires interface implementation registration
func TestBtcdNotifier(t *testing.T) {

	// Initialize the harness around a btcd node which will serve as our
	// dedicated miner to generate blocks, cause re-orgs, etc. We'll set
	// up this node with a chain length of 125, so we have plentyyy of BTC
	// to play around with.
	miner, err := rpctest.New(netParams, nil, nil)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	defer miner.TearDown()
	if err := miner.SetUp(true, 25); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}

	nodeConfig := miner.RPCConfig()
	notifier, err := NewBtcdNotifier(&nodeConfig)
	if err != nil {
		t.Fatalf("unable to create notifier: %v", err)
	}
	if err := notifier.Start(); err != nil {
		t.Fatalf("unable to start notifier: %v", err)
	}
	defer notifier.Stop()

	for _, ntfnTest := range ntfnTests {
		ntfnTest(miner, notifier, t)
	}
}
