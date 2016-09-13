package chainntnfs_test

import (
	"bytes"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	_ "github.com/lightningnetwork/lnd/chainntnfs/btcdnotify"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
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
	script, err := txscript.PayToAddrScript(testAddr)
	if err != nil {
		return nil, err
	}

	outputs := []*wire.TxOut{&wire.TxOut{2e8, script}}
	return miner.CoinbaseSpend(outputs)
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
		t.Fatalf("unable to create test tx: %v", err)
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

	confSent := make(chan int32)
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

	confSent := make(chan int32)
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

		confSent := make(chan int32)
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
	// ChainNotifier concrete implemenations.
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

func testBlockEpochNotification(miner *rpctest.Harness,
	notifier chainntnfs.ChainNotifier, t *testing.T) {

	// We'd like to test the case of multiple registered clients receiving
	// block epoch notifications.

	const numBlocks = 10
	const numClients = 5
	var wg sync.WaitGroup

	// Create numClients clients which will listen for block notifications. We
	// expect each client to receive 10 notifications for each of the ten
	// blocks we generate below. So we'll use a WaitGroup to synchronize the
	// test.
	for i := 0; i < numClients; i++ {
		epochClient, err := notifier.RegisterBlockEpochNtfn()
		if err != nil {
			t.Fatalf("unable to register for epoch notification")
		}

		wg.Add(numBlocks)
		go func() {
			for i := 0; i < numBlocks; i++ {
				<-epochClient.Epochs
				wg.Done()
			}
		}()
	}

	epochsSent := make(chan struct{})
	go func() {
		wg.Wait()
		close(epochsSent)
	}()

	// Now generate 10 blocks, the clients above should each receive 10
	// notifications, thereby unblocking the goroutine above.
	if _, err := miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	select {
	case <-epochsSent:
	case <-time.After(2 * time.Second):
		t.Fatalf("all notifications not sent")
	}
}

func testMultiClientConfirmationNotification(miner *rpctest.Harness,
	notifier chainntnfs.ChainNotifier, t *testing.T) {
	// TODO(roasbeef): test various conf targets w/ same txid

	// We'd like to test the case of a multiple clients registered to
	// receive a confirmation notification for the same transaction.

	txid, err := getTestTxId(miner)
	if err != nil {
		t.Fatalf("unable to create test tx: %v", err)
	}

	var wg sync.WaitGroup
	const numConfsClients = 5
	const numConfs = 1

	// Register for a conf notification for the above generated txid with
	// numConfsClients distinct clients.
	for i := 0; i < numConfsClients; i++ {
		confClient, err := notifier.RegisterConfirmationsNtfn(txid, numConfs)
		if err != nil {
			t.Fatalf("unable to register for confirmation: %v", err)
		}

		wg.Add(1)
		go func() {
			<-confClient.Confirmed
			wg.Done()
		}()
	}

	confsSent := make(chan struct{})
	go func() {
		wg.Wait()
		close(confsSent)
	}()

	// Finally, generate a single block which should trigger the unblocking
	// of all numConfsClients blocked on the channel read above.
	if _, err := miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	select {
	case <-confsSent:
	case <-time.After(2 * time.Second):
		t.Fatalf("all confirmation notifications not sent")
	}
}

var ntfnTests = []func(node *rpctest.Harness, notifier chainntnfs.ChainNotifier, t *testing.T){
	testSingleConfirmationNotification,
	testMultiConfirmationNotification,
	testBatchConfirmationNotification,
	testMultiClientConfirmationNotification,
	testSpendNotification,
	testBlockEpochNotification,
}

// TestInterfaces tests all registered interfaces with a unified set of tests
// which excersie each of the required methods found within the ChainNotifier
// interface.
//
// NOTE: In the future, when additional implementations of the ChainNotifier
// interface have been implemented, in order to ensure the new concrete
// implementation is automatically tested, two steps must be undertaken. First,
// one needs add a "non-captured" (_) import from the new sub-package. This
// import should trigger an init() method within the package which registeres
// the interface. Second, an additional case in the switch within the main loop
// below needs to be added which properly initializes the interface.
func TestInterfaces(t *testing.T) {
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

	log.Printf("Running %v ChainNotifier interface tests\n", len(ntfnTests))
	var notifier chainntnfs.ChainNotifier
	for _, notifierDriver := range chainntnfs.RegisteredNotifiers() {
		notifierType := notifierDriver.NotifierType

		switch notifierType {
		case "btcd":
			notifier, err = notifierDriver.New(&rpcConfig)
			if err != nil {
				t.Fatalf("unable to create %v notifier: %v",
					notifierType, err)
			}
		}

		if err := notifier.Start(); err != nil {
			t.Fatalf("unable to start notifier %v: %v",
				notifierType, err)
		}

		for _, ntfnTest := range ntfnTests {
			ntfnTest(miner, notifier, t)
		}

		notifier.Stop()
	}
}
