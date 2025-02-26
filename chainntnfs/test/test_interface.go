//go:build dev
// +build dev

package chainntnfstest

import (
	"bytes"
	"fmt"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb" // Required to auto-register the boltdb walletdb implementation.
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainntnfs/bitcoindnotify"
	"github.com/lightningnetwork/lnd/chainntnfs/btcdnotify"
	"github.com/lightningnetwork/lnd/chainntnfs/neutrinonotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/stretchr/testify/require"
)

func testSingleConfirmationNotification(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// We'd like to test the case of being notified once a txid reaches
	// a *single* confirmation.
	//
	// So first, let's send some coins to "ourself", obtaining a txid.
	// We're spending from a coinbase output here, so we use the dedicated
	// function.
	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	err = chainntnfs.WaitForMempoolTx(miner, txid)
	require.NoError(t, err, "tx not relayed to miner")

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Now that we have a txid, register a confirmation notification with
	// the chainntfn source.
	numConfs := uint32(1)
	var confIntent *chainntnfs.ConfirmationEvent
	if scriptDispatch {
		confIntent, err = notifier.RegisterConfirmationsNtfn(
			nil, pkScript, numConfs, uint32(currentHeight),
		)
	} else {
		confIntent, err = notifier.RegisterConfirmationsNtfn(
			txid, pkScript, numConfs, uint32(currentHeight),
		)
	}
	require.NoError(t, err, "unable to register ntfn")

	// Now generate a single block, the transaction should be included which
	// should trigger a notification event.
	blockHash, err := miner.Client.Generate(1)
	require.NoError(t, err, "unable to generate single block")

	// Assert the above tx is mined in the block.
	block, err := miner.Client.GetBlock(blockHash[0])
	require.NoError(t, err)
	require.Len(t, block.Transactions, 2, "block does not contain tx")

	select {
	case confInfo := <-confIntent.Confirmed:
		if !confInfo.BlockHash.IsEqual(blockHash[0]) {
			t.Fatalf("mismatched block hashes: expected %v, got %v",
				blockHash[0], confInfo.BlockHash)
		}

		// Finally, we'll verify that the tx index returned is the exact same
		// as the tx index of the transaction within the block itself.
		msgBlock, err := miner.Client.GetBlock(blockHash[0])
		if err != nil {
			t.Fatalf("unable to fetch block: %v", err)
		}

		block := btcutil.NewBlock(msgBlock)
		specifiedTxHash, err := block.TxHash(int(confInfo.TxIndex))
		if err != nil {
			t.Fatalf("unable to index into block: %v", err)
		}

		if !specifiedTxHash.IsEqual(txid) {
			t.Fatalf("mismatched tx indexes: expected %v, got %v",
				txid, specifiedTxHash)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("confirmation notification never received")
	}
}

func testMultiConfirmationNotification(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// We'd like to test the case of being notified once a txid reaches
	// N confirmations, where N > 1.
	//
	// Again, we'll begin by creating a fresh transaction, so we can obtain
	// a fresh txid.
	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test addr")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	numConfs := uint32(6)
	var confIntent *chainntnfs.ConfirmationEvent
	if scriptDispatch {
		confIntent, err = notifier.RegisterConfirmationsNtfn(
			nil, pkScript, numConfs, uint32(currentHeight),
		)
	} else {
		confIntent, err = notifier.RegisterConfirmationsNtfn(
			txid, pkScript, numConfs, uint32(currentHeight),
		)
	}
	require.NoError(t, err, "unable to register ntfn")

	// Now generate a six blocks. The transaction should be included in the
	// first block, which will be built upon by the other 5 blocks.
	if _, err := miner.Client.Generate(6); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	// TODO(roasbeef): reduce all timeouts after neutrino sync tightended
	// up

	select {
	case <-confIntent.Confirmed:
		break
	case <-time.After(20 * time.Second):
		t.Fatalf("confirmation notification never received")
	}
}

func testBatchConfirmationNotification(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// We'd like to test a case of serving notifications to multiple
	// clients, each requesting to be notified once a txid receives
	// various numbers of confirmations.
	confSpread := [6]uint32{1, 2, 3, 6, 20, 22}
	confIntents := make([]*chainntnfs.ConfirmationEvent, len(confSpread))

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Create a new txid spending miner coins for each confirmation entry
	// in confSpread, we collect each conf intent into a slice so we can
	// verify they're each notified at the proper number of confirmations
	// below.
	for i, numConfs := range confSpread {
		// All the clients with an even index will ask for the block
		// along side the conf ntfn.
		var opts []chainntnfs.NotifierOption
		if i%2 == 0 {
			opts = append(opts, chainntnfs.WithIncludeBlock())
		}

		txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
		if err != nil {
			t.Fatalf("unable to create test addr: %v", err)
		}
		var confIntent *chainntnfs.ConfirmationEvent
		if scriptDispatch {
			confIntent, err = notifier.RegisterConfirmationsNtfn(
				nil, pkScript, numConfs, uint32(currentHeight),
				opts...,
			)
		} else {
			confIntent, err = notifier.RegisterConfirmationsNtfn(
				txid, pkScript, numConfs, uint32(currentHeight),
				opts...,
			)
		}
		if err != nil {
			t.Fatalf("unable to register ntfn: %v", err)
		}
		confIntents[i] = confIntent
		if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
			t.Fatalf("tx not relayed to miner: %v", err)
		}

	}

	initialConfHeight := uint32(currentHeight + 1)

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
		if _, err := miner.Client.Generate(blocksToGen); err != nil {
			t.Fatalf("unable to generate single block: %v", err)
		}

		select {
		case conf := <-confIntents[i].Confirmed:
			// All of the notifications above were originally
			// confirmed in the same block. The returned
			// notification should list the initial confirmation
			// height rather than the height they were _fully_
			// confirmed.
			if conf.BlockHeight != initialConfHeight {
				t.Fatalf("notification has incorrect initial "+
					"conf height: expected %v, got %v",
					initialConfHeight, conf.BlockHeight)
			}

			// If this is an even client index, then we expect the
			// block to be populated. Otherwise, it should be
			// empty.
			expectBlock := i%2 == 0
			require.Equal(t, expectBlock, conf.Block != nil)
			continue
		case <-time.After(20 * time.Second):
			t.Fatalf("confirmation notification never received: %v", numConfs)
		}
	}
}

func checkNotificationFields(ntfn *chainntnfs.SpendDetail,
	outpoint *wire.OutPoint, spenderSha *chainhash.Hash,
	height int32, t *testing.T) {

	t.Helper()

	if *ntfn.SpentOutPoint != *outpoint {
		t.Fatalf("ntfn includes wrong output, reports "+
			"%v instead of %v",
			ntfn.SpentOutPoint, outpoint)
	}
	if !bytes.Equal(ntfn.SpenderTxHash[:], spenderSha[:]) {
		t.Fatalf("ntfn includes wrong spender tx sha, "+
			"reports %v instead of %v",
			ntfn.SpenderTxHash[:], spenderSha[:])
	}
	if ntfn.SpenderInputIndex != 0 {
		t.Fatalf("ntfn includes wrong spending input "+
			"index, reports %v, should be %v",
			ntfn.SpenderInputIndex, 0)
	}
	if ntfn.SpendingHeight != height {
		t.Fatalf("ntfn has wrong spending height: "+
			"expected %v, got %v", height,
			ntfn.SpendingHeight)
	}
}

func testSpendNotification(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// We'd like to test the spend notifications for all ChainNotifier
	// concrete implementations.
	//
	// To do so, we first create a new output to our test target address.
	outpoint, output, privKey := chainntnfs.CreateSpendableOutput(t, miner)

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Now that we have an output index and the pkScript, register for a
	// spentness notification for the newly created output with multiple
	// clients in order to ensure the implementation can support
	// multi-client spend notifications.
	const numClients = 5
	spendClients := make([]*chainntnfs.SpendEvent, numClients)
	for i := 0; i < numClients; i++ {
		var spentIntent *chainntnfs.SpendEvent
		if scriptDispatch {
			spentIntent, err = notifier.RegisterSpendNtfn(
				nil, output.PkScript, uint32(currentHeight),
			)
		} else {
			spentIntent, err = notifier.RegisterSpendNtfn(
				outpoint, output.PkScript, uint32(currentHeight),
			)
		}
		if err != nil {
			t.Fatalf("unable to register for spend ntfn: %v", err)
		}

		spendClients[i] = spentIntent
	}

	// Next, create a new transaction spending that output.
	spendingTx := chainntnfs.CreateSpendTx(t, outpoint, output, privKey)

	// Broadcast our spending transaction.
	spenderSha, err := miner.Client.SendRawTransaction(spendingTx, true)
	require.NoError(t, err, "unable to broadcast tx")

	if err := chainntnfs.WaitForMempoolTx(miner, spenderSha); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	// Make sure notifications are not yet sent. We launch a go routine for
	// all the spend clients, such that we can wait for them all in
	// parallel.
	mempoolSpendTimeout := 2 * chainntnfs.TrickleInterval
	mempoolSpends := make(chan *chainntnfs.SpendDetail, numClients)
	for _, c := range spendClients {
		go func(client *chainntnfs.SpendEvent) {
			select {
			case s := <-client.Spend:
				mempoolSpends <- s
			case <-time.After(mempoolSpendTimeout):
			}
		}(c)
	}

	select {
	case <-mempoolSpends:
		t.Fatalf("did not expect to get notification before " +
			"block was mined")
	case <-time.After(mempoolSpendTimeout):
	}

	// Make sure registering a client after the tx is in the mempool still
	// doesn't trigger a notification.
	var spentIntent *chainntnfs.SpendEvent
	if scriptDispatch {
		spentIntent, err = notifier.RegisterSpendNtfn(
			nil, output.PkScript, uint32(currentHeight),
		)
	} else {
		spentIntent, err = notifier.RegisterSpendNtfn(
			outpoint, output.PkScript, uint32(currentHeight),
		)
	}
	require.NoError(t, err, "unable to register for spend ntfn")

	select {
	case <-spentIntent.Spend:
		t.Fatalf("did not expect to get notification before " +
			"block was mined")
	case <-time.After(mempoolSpendTimeout):
	}
	spendClients = append(spendClients, spentIntent)

	// Now we mine a single block, which should include our spend. The
	// notification should also be sent off.
	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	_, currentHeight, err = miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	for _, c := range spendClients {
		select {
		case ntfn := <-c.Spend:
			// We've received the spend nftn. So now verify all the
			// fields have been set properly.
			checkNotificationFields(ntfn, outpoint, spenderSha,
				currentHeight, t)
		case <-time.After(30 * time.Second):
			t.Fatalf("spend ntfn never received")
		}
	}
}

func testBlockEpochNotification(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, t *testing.T) {

	// We'd like to test the case of multiple registered clients receiving
	// block epoch notifications.
	const numBlocks = 10
	const numNtfns = numBlocks + 1
	const numClients = 5
	var wg sync.WaitGroup

	// Create numClients clients which will listen for block notifications. We
	// expect each client to receive 11 notifications, one for the current
	// tip of the chain, and one for each of the ten blocks we generate
	// below. So we'll use a WaitGroup to synchronize the test.
	clientErrors := make(chan error, numClients)
	for i := 0; i < numClients; i++ {
		epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
		if err != nil {
			t.Fatalf("unable to register for epoch notification")
		}

		wg.Add(numNtfns)
		go func() {
			for i := 0; i < numNtfns; i++ {
				// Ensure that each block epoch has a header,
				// and that header matches the contained header
				// hash.
				blockEpoch := <-epochClient.Epochs
				if blockEpoch.BlockHeader == nil {
					t.Logf("%d", i)
					clientErrors <- fmt.Errorf("block " +
						"header is nil")
					return
				}
				if blockEpoch.BlockHeader.BlockHash() !=
					*blockEpoch.Hash {

					clientErrors <- fmt.Errorf("block " +
						"header hash mismatch")
					return
				}

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
	if _, err := miner.Client.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	select {
	case err := <-clientErrors:
		t.Fatalf("block epoch case failed: %v", err)
	case <-epochsSent:
	case <-time.After(30 * time.Second):
		t.Fatalf("all notifications not sent")
	}
}

func testMultiClientConfirmationNotification(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// We'd like to test the case of a multiple clients registered to
	// receive a confirmation notification for the same transaction.
	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	var wg sync.WaitGroup
	const (
		numConfsClients = 5
		numConfs        = 1
	)

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Register for a conf notification for the above generated txid with
	// numConfsClients distinct clients.
	for i := 0; i < numConfsClients; i++ {
		var confClient *chainntnfs.ConfirmationEvent
		if scriptDispatch {
			confClient, err = notifier.RegisterConfirmationsNtfn(
				nil, pkScript, numConfs, uint32(currentHeight),
			)
		} else {
			confClient, err = notifier.RegisterConfirmationsNtfn(
				txid, pkScript, numConfs, uint32(currentHeight),
			)
		}
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
	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	select {
	case <-confsSent:
	case <-time.After(30 * time.Second):
		t.Fatalf("all confirmation notifications not sent")
	}
}

// Tests the case in which a confirmation notification is requested for a
// transaction that has already been included in a block. In this case, the
// confirmation notification should be dispatched immediately.
func testTxConfirmedBeforeNtfnRegistration(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// First, let's send some coins to "ourself", obtaining a txid.  We're
	// spending from a coinbase output here, so we use the dedicated
	// function.
	txid3, pkScript3, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid3); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	// Generate another block containing tx 3, but we won't register conf
	// notifications for this tx until much later. The notifier must check
	// older blocks when the confirmation event is registered below to ensure
	// that the TXID hasn't already been included in the chain, otherwise the
	// notification will never be sent.
	_, err = miner.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")

	txid1, pkScript1, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid1); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	txid2, pkScript2, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid2); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Now generate another block containing txs 1 & 2.
	blockHash, err := miner.Client.Generate(1)
	require.NoError(t, err, "unable to generate block")

	// Register a confirmation notification with the chainntfn source for tx2,
	// which is included in the last block. The height hint is the height before
	// the block is included. This notification should fire immediately since
	// only 1 confirmation is required.
	var ntfn1 *chainntnfs.ConfirmationEvent
	if scriptDispatch {
		ntfn1, err = notifier.RegisterConfirmationsNtfn(
			nil, pkScript1, 1, uint32(currentHeight),
			chainntnfs.WithIncludeBlock(),
		)
	} else {
		ntfn1, err = notifier.RegisterConfirmationsNtfn(
			txid1, pkScript1, 1, uint32(currentHeight),
		)
	}
	require.NoError(t, err, "unable to register ntfn")

	select {
	case confInfo := <-ntfn1.Confirmed:
		// Finally, we'll verify that the tx index returned is the exact same
		// as the tx index of the transaction within the block itself.
		msgBlock, err := miner.Client.GetBlock(blockHash[0])
		if err != nil {
			t.Fatalf("unable to fetch block: %v", err)
		}
		block := btcutil.NewBlock(msgBlock)
		specifiedTxHash, err := block.TxHash(int(confInfo.TxIndex))
		if err != nil {
			t.Fatalf("unable to index into block: %v", err)
		}
		if !specifiedTxHash.IsEqual(txid1) {
			t.Fatalf("mismatched tx indexes: expected %v, got %v",
				txid1, specifiedTxHash)
		}

		// We'll also ensure that the block height has been set
		// properly.
		if confInfo.BlockHeight != uint32(currentHeight+1) {
			t.Fatalf("incorrect block height: expected %v, got %v",
				confInfo.BlockHeight, currentHeight)
		}

		// Ensure that if this was a script dispatch, the block is set
		// as well.
		if scriptDispatch {
			require.NotNil(t, confInfo.Block)
		}

		break
	case <-time.After(20 * time.Second):
		t.Fatalf("confirmation notification never received")
	}

	// Register a confirmation notification for tx2, requiring 3 confirmations.
	// This transaction is only partially confirmed, so the notification should
	// not fire yet.
	var ntfn2 *chainntnfs.ConfirmationEvent
	if scriptDispatch {
		ntfn2, err = notifier.RegisterConfirmationsNtfn(
			nil, pkScript2, 3, uint32(currentHeight),
			chainntnfs.WithIncludeBlock(),
		)
	} else {
		ntfn2, err = notifier.RegisterConfirmationsNtfn(
			txid2, pkScript2, 3, uint32(currentHeight),
		)
	}
	require.NoError(t, err, "unable to register ntfn")

	// Fully confirm tx3.
	_, err = miner.Client.Generate(2)
	require.NoError(t, err, "unable to generate block")

	select {
	case <-ntfn2.Confirmed:
	case <-time.After(10 * time.Second):
		t.Fatalf("confirmation notification never received")
	}

	select {
	case <-ntfn1.Confirmed:
		t.Fatalf("received multiple confirmations for tx")
	case <-time.After(1 * time.Second):
	}

	// Finally register a confirmation notification for tx3, requiring 1
	// confirmation. Ensure that conf notifications do not refire on txs
	// 1 or 2.
	var ntfn3 *chainntnfs.ConfirmationEvent
	if scriptDispatch {
		ntfn3, err = notifier.RegisterConfirmationsNtfn(
			nil, pkScript3, 1, uint32(currentHeight-1),
			chainntnfs.WithIncludeBlock(),
		)
	} else {
		ntfn3, err = notifier.RegisterConfirmationsNtfn(
			txid3, pkScript3, 1, uint32(currentHeight-1),
		)
	}
	require.NoError(t, err, "unable to register ntfn")

	// We'll also register for a confirmation notification with the pkscript
	// of a different transaction. This notification shouldn't fire since we
	// match on both txid and pkscript.
	var ntfn4 *chainntnfs.ConfirmationEvent
	ntfn4, err = notifier.RegisterConfirmationsNtfn(
		txid3, pkScript2, 1, uint32(currentHeight-1),
	)
	require.NoError(t, err, "unable to register ntfn")

	select {
	case confInfo := <-ntfn3.Confirmed:
		if scriptDispatch {
			require.NotNil(t, confInfo.Block)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("confirmation notification never received")
	}

	select {
	case <-ntfn4.Confirmed:
		t.Fatalf("confirmation notification received")
	case <-time.After(5 * time.Second):
	}

	time.Sleep(1 * time.Second)

	select {
	case <-ntfn1.Confirmed:
		t.Fatalf("received multiple confirmations for tx")
	default:
	}

	select {
	case <-ntfn2.Confirmed:
		t.Fatalf("received multiple confirmations for tx")
	default:
	}
}

// Test the case of a notification consumer having forget or being delayed in
// checking for a confirmation. This should not cause the notifier to stop
// working
func testLazyNtfnConsumer(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// Create a transaction to be notified about. We'll register for
	// notifications on this transaction but won't be prompt in checking them
	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	numConfs := uint32(3)

	// Add a block right before registering, this makes race conditions
	// between the historical dispatcher and the normal dispatcher more obvious
	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	var firstConfIntent *chainntnfs.ConfirmationEvent
	if scriptDispatch {
		firstConfIntent, err = notifier.RegisterConfirmationsNtfn(
			nil, pkScript, numConfs, uint32(currentHeight),
		)
	} else {
		firstConfIntent, err = notifier.RegisterConfirmationsNtfn(
			txid, pkScript, numConfs, uint32(currentHeight),
		)
	}
	require.NoError(t, err, "unable to register ntfn")

	// Generate another 2 blocks, this should dispatch the confirm notification
	if _, err := miner.Client.Generate(2); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Now make another transaction, just because we haven't checked to see
	// if the first transaction has confirmed doesn't mean that we shouldn't
	// be able to see if this transaction confirms first
	txid, pkScript, err = chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	_, currentHeight, err = miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	numConfs = 1
	var secondConfIntent *chainntnfs.ConfirmationEvent
	if scriptDispatch {
		secondConfIntent, err = notifier.RegisterConfirmationsNtfn(
			nil, pkScript, numConfs, uint32(currentHeight),
		)
	} else {
		secondConfIntent, err = notifier.RegisterConfirmationsNtfn(
			txid, pkScript, numConfs, uint32(currentHeight),
		)
	}
	require.NoError(t, err, "unable to register ntfn")

	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	select {
	case <-secondConfIntent.Confirmed:
		// Successfully receive the second notification
		break
	case <-time.After(30 * time.Second):
		t.Fatalf("Second confirmation notification never received")
	}

	// Make sure the first tx confirmed successfully
	select {
	case <-firstConfIntent.Confirmed:
		break
	case <-time.After(30 * time.Second):
		t.Fatalf("First confirmation notification never received")
	}
}

// Tests the case in which a spend notification is requested for a spend that
// has already been included in a block. In this case, the spend notification
// should be dispatched immediately.
func testSpendBeforeNtfnRegistration(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// We'd like to test the spend notifications for all ChainNotifier
	// concrete implementations.
	//
	// To do so, we first create a new output to our test target address.
	outpoint, output, privKey := chainntnfs.CreateSpendableOutput(t, miner)

	_, heightHint, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// We'll then spend this output and broadcast the spend transaction.
	spendingTx := chainntnfs.CreateSpendTx(t, outpoint, output, privKey)
	spenderSha, err := miner.Client.SendRawTransaction(spendingTx, true)
	require.NoError(t, err, "unable to broadcast tx")
	if err := chainntnfs.WaitForMempoolTx(miner, spenderSha); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	// We create an epoch client we can use to make sure the notifier is
	// caught up to the mining node's chain.
	epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err, "unable to register for block epoch")

	// Now we mine an additional block, which should include our spend.
	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}
	_, spendHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// checkSpends registers two clients to be notified of a spend that has
	// already happened. The notifier should dispatch a spend notification
	// immediately.
	checkSpends := func() {
		t.Helper()

		const numClients = 2
		spendClients := make([]*chainntnfs.SpendEvent, numClients)
		for i := 0; i < numClients; i++ {
			var spentIntent *chainntnfs.SpendEvent
			if scriptDispatch {
				spentIntent, err = notifier.RegisterSpendNtfn(
					nil, output.PkScript, uint32(heightHint),
				)
			} else {
				spentIntent, err = notifier.RegisterSpendNtfn(
					outpoint, output.PkScript,
					uint32(heightHint),
				)
			}
			if err != nil {
				t.Fatalf("unable to register for spend ntfn: %v",
					err)
			}

			spendClients[i] = spentIntent
		}

		for _, client := range spendClients {
			select {
			case ntfn := <-client.Spend:
				// We've received the spend nftn. So now verify
				// all the fields have been set properly.
				checkNotificationFields(
					ntfn, outpoint, spenderSha, spendHeight, t,
				)
			case <-time.After(30 * time.Second):
				t.Fatalf("spend ntfn never received")
			}
		}
	}

	// Wait for the notifier to have caught up to the mined block.
	select {
	case _, ok := <-epochClient.Epochs:
		if !ok {
			t.Fatalf("epoch channel was closed")
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("did not receive block epoch")
	}

	// Check that the spend clients gets immediately notified for the spend
	// in the previous block.
	checkSpends()

	// Bury the spend even deeper, and do the same check.
	const numBlocks = 10
	if _, err := miner.Client.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	// Wait for the notifier to have caught up with the new blocks.
	for i := 0; i < numBlocks; i++ {
		select {
		case _, ok := <-epochClient.Epochs:
			if !ok {
				t.Fatalf("epoch channel was closed")
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("did not receive block epoch")
		}
	}

	// The clients should still be notified immediately.
	checkSpends()
}

func testCancelSpendNtfn(node *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// We'd like to test that once a spend notification is registered, it
	// can be canceled before the notification is dispatched.

	// First, we'll start by creating a new output that we can spend
	// ourselves.
	outpoint, output, privKey := chainntnfs.CreateSpendableOutput(t, node)

	_, currentHeight, err := node.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Create two clients that each registered to the spend notification.
	// We'll cancel the notification for the first client and leave the
	// notification for the second client enabled.
	const numClients = 2
	spendClients := make([]*chainntnfs.SpendEvent, numClients)
	for i := 0; i < numClients; i++ {
		var spentIntent *chainntnfs.SpendEvent
		if scriptDispatch {
			spentIntent, err = notifier.RegisterSpendNtfn(
				nil, output.PkScript, uint32(currentHeight),
			)
		} else {
			spentIntent, err = notifier.RegisterSpendNtfn(
				outpoint, output.PkScript, uint32(currentHeight),
			)
		}
		if err != nil {
			t.Fatalf("unable to register for spend ntfn: %v", err)
		}

		spendClients[i] = spentIntent
	}

	// Next, create a new transaction spending that output.
	spendingTx := chainntnfs.CreateSpendTx(t, outpoint, output, privKey)

	// Before we broadcast the spending transaction, we'll cancel the
	// notification of the first client.
	spendClients[1].Cancel()

	// Broadcast our spending transaction.
	spenderSha, err := node.Client.SendRawTransaction(spendingTx, true)
	require.NoError(t, err, "unable to broadcast tx")

	if err := chainntnfs.WaitForMempoolTx(node, spenderSha); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	// Now we mine a single block, which should include our spend. The
	// notification should also be sent off.
	if _, err := node.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	// The spend notification for the first client should have been
	// dispatched.
	select {
	case ntfn := <-spendClients[0].Spend:
		// We've received the spend nftn. So now verify all the
		// fields have been set properly.
		if *ntfn.SpentOutPoint != *outpoint {
			t.Fatalf("ntfn includes wrong output, reports "+
				"%v instead of %v",
				ntfn.SpentOutPoint, outpoint)
		}
		if !bytes.Equal(ntfn.SpenderTxHash[:], spenderSha[:]) {
			t.Fatalf("ntfn includes wrong spender tx sha, "+
				"reports %v instead of %v",
				ntfn.SpenderTxHash[:], spenderSha[:])
		}
		if ntfn.SpenderInputIndex != 0 {
			t.Fatalf("ntfn includes wrong spending input "+
				"index, reports %v, should be %v",
				ntfn.SpenderInputIndex, 0)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("spend ntfn never received")
	}

	// However, the spend notification of the second client should NOT have
	// been dispatched.
	select {
	case _, ok := <-spendClients[1].Spend:
		if ok {
			t.Fatalf("spend ntfn should have been canceled")
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("spend ntfn never canceled")
	}
}

func testCancelEpochNtfn(node *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, t *testing.T) {

	// We'd like to ensure that once a client cancels their block epoch
	// notifications, no further notifications are sent over the channel
	// if/when new blocks come in.
	const numClients = 2

	epochClients := make([]*chainntnfs.BlockEpochEvent, numClients)
	for i := 0; i < numClients; i++ {
		epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
		if err != nil {
			t.Fatalf("unable to register for epoch notification")
		}
		epochClients[i] = epochClient
	}

	// Now before we mine any blocks, cancel the notification for the first
	// epoch client.
	epochClients[0].Cancel()

	// Now mine a single block, this should trigger the logic to dispatch
	// epoch notifications.
	if _, err := node.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// The epoch notification for the first client shouldn't have been
	// dispatched.
	select {
	case _, ok := <-epochClients[0].Epochs:
		if ok {
			t.Fatalf("epoch notification should have been canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("epoch notification not sent")
	}

	// However, the epoch notification for the second client should have
	// been dispatched as normal.
	select {
	case _, ok := <-epochClients[1].Epochs:
		if !ok {
			t.Fatalf("epoch was canceled")
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("epoch notification not sent")
	}
}

func testReorgConf(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// Set up a new miner that we can use to cause a reorg.
	miner2 := unittest.NewMiner(
		t, unittest.NetParams, []string{"--txindex"}, false, 0,
	)

	// We start by connecting the new miner to our original miner,
	// such that it will sync to our original chain.
	if err := rpctest.ConnectNode(miner, miner2); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice := []*rpctest.Harness{miner, miner2}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// The two should be on the same blockheight.
	_, nodeHeight1, err := miner.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, nodeHeight2, err := miner2.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if nodeHeight1 != nodeHeight2 {
		t.Fatalf("expected both miners to be on the same height: %v vs %v",
			nodeHeight1, nodeHeight2)
	}

	// We disconnect the two nodes, such that we can start mining on them
	// individually without the other one learning about the new blocks.
	err = miner.Client.AddNode(miner2.P2PAddress(), rpcclient.ANRemove)
	require.NoError(t, err, "unable to remove node")

	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	// Now that we have a txid, register a confirmation notification with
	// the chainntfn source.
	numConfs := uint32(2)
	var confIntent *chainntnfs.ConfirmationEvent
	if scriptDispatch {
		confIntent, err = notifier.RegisterConfirmationsNtfn(
			nil, pkScript, numConfs, uint32(currentHeight),
		)
	} else {
		confIntent, err = notifier.RegisterConfirmationsNtfn(
			txid, pkScript, numConfs, uint32(currentHeight),
		)
	}
	require.NoError(t, err, "unable to register ntfn")

	// Now generate a single block, the transaction should be included.
	_, err = miner.Client.Generate(1)
	require.NoError(t, err, "unable to generate single block")

	// Transaction only has one confirmation, and the notification is registered
	// with 2 confirmations, so we should not be notified yet.
	select {
	case <-confIntent.Confirmed:
		t.Fatal("tx was confirmed unexpectedly")
	case <-time.After(1 * time.Second):
	}

	// Reorganize transaction out of the chain by generating a longer fork
	// from the other miner. The transaction is not included in this fork.
	_, err = miner2.Client.Generate(2)
	require.NoError(t, err)

	// Reconnect nodes to reach consensus on the longest chain. miner2's chain
	// should win and become active on miner1.
	if err := rpctest.ConnectNode(miner, miner2); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice = []*rpctest.Harness{miner, miner2}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	_, nodeHeight1, err = miner.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, nodeHeight2, err = miner2.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if nodeHeight1 != nodeHeight2 {
		t.Fatalf("expected both miners to be on the same height: %v vs %v",
			nodeHeight1, nodeHeight2)
	}

	// Even though there is one block above the height of the block that the
	// transaction was included in, it is not the active chain so the
	// notification should not be sent.
	select {
	case <-confIntent.Confirmed:
		t.Fatal("tx was confirmed unexpectedly")
	case <-time.After(1 * time.Second):
	}

	// Now confirm the transaction on the longest chain and verify that we
	// receive the notification.
	tx, err := miner.Client.GetRawTransaction(txid)
	require.NoError(t, err, "unable to get raw tx")

	txid, err = miner2.Client.SendRawTransaction(tx.MsgTx(), false)
	require.NoError(t, err, "unable to get send tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	_, err = miner.Client.Generate(3)
	require.NoError(t, err, "unable to generate single block")

	select {
	case <-confIntent.Confirmed:
	case <-time.After(20 * time.Second):
		t.Fatalf("confirmation notification never received")
	}
}

// testReorgSpend ensures that the different ChainNotifier implementations
// correctly handle outpoints whose spending transaction has been reorged out of
// the chain.
func testReorgSpend(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool, t *testing.T) {

	// We'll start by creating an output and registering a spend
	// notification for it.
	outpoint, output, privKey := chainntnfs.CreateSpendableOutput(t, miner)
	_, heightHint, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve current height")

	var spendIntent *chainntnfs.SpendEvent
	if scriptDispatch {
		spendIntent, err = notifier.RegisterSpendNtfn(
			nil, output.PkScript, uint32(heightHint),
		)
	} else {
		spendIntent, err = notifier.RegisterSpendNtfn(
			outpoint, output.PkScript, uint32(heightHint),
		)
	}
	require.NoError(t, err, "unable to register for spend")

	// Set up a new miner that we can use to cause a reorg.
	miner2 := unittest.NewMiner(
		t, unittest.NetParams, []string{"--txindex"}, false, 0,
	)

	// We start by connecting the new miner to our original miner, in order
	// to have a consistent view of the chain from both miners. They should
	// be on the same block height.
	if err := rpctest.ConnectNode(miner, miner2); err != nil {
		t.Fatalf("unable to connect miners: %v", err)
	}
	nodeSlice := []*rpctest.Harness{miner, miner2}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to sync miners: %v", err)
	}
	_, minerHeight1, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get miner1's current height")
	_, minerHeight2, err := miner2.Client.GetBestBlock()
	require.NoError(t, err, "unable to get miner2's current height")
	if minerHeight1 != minerHeight2 {
		t.Fatalf("expected both miners to be on the same height: "+
			"%v vs %v", minerHeight1, minerHeight2)
	}

	// We disconnect the two nodes, such that we can start mining on them
	// individually without the other one learning about the new blocks.
	err = miner.Client.AddNode(miner2.P2PAddress(), rpcclient.ANRemove)
	require.NoError(t, err, "unable to disconnect miners")

	// Craft the spending transaction for the outpoint created above and
	// confirm it under the chain of the original miner.
	spendTx := chainntnfs.CreateSpendTx(t, outpoint, output, privKey)
	spendTxHash, err := miner.Client.SendRawTransaction(spendTx, true)
	require.NoError(t, err, "unable to broadcast spend tx")
	if err := chainntnfs.WaitForMempoolTx(miner, spendTxHash); err != nil {
		t.Fatalf("spend tx not relayed to miner: %v", err)
	}
	const numBlocks = 1
	if _, err := miner.Client.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}
	_, spendHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get spend height")

	// We should see a spend notification dispatched with the correct spend
	// details.
	select {
	case spendDetails := <-spendIntent.Spend:
		checkNotificationFields(
			spendDetails, outpoint, spendTxHash, spendHeight, t,
		)
	case <-time.After(5 * time.Second):
		t.Fatal("expected spend notification to be dispatched")
	}

	// Now, with the other miner, we'll generate one more block than the
	// other miner and connect them to cause a reorg.
	if _, err := miner2.Client.Generate(numBlocks + 1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}
	if err := rpctest.ConnectNode(miner, miner2); err != nil {
		t.Fatalf("unable to connect miners: %v", err)
	}
	nodeSlice = []*rpctest.Harness{miner2, miner}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to sync miners: %v", err)
	}
	_, minerHeight1, err = miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get miner1's current height")
	_, minerHeight2, err = miner2.Client.GetBestBlock()
	require.NoError(t, err, "unable to get miner2's current height")
	if minerHeight1 != minerHeight2 {
		t.Fatalf("expected both miners to be on the same height: "+
			"%v vs %v", minerHeight1, minerHeight2)
	}

	// We should receive a reorg notification.
	select {
	case _, ok := <-spendIntent.Reorg:
		if !ok {
			t.Fatal("unexpected reorg channel closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected to receive reorg notification")
	}

	// Now that both miners are on the same chain, we'll confirm the
	// spending transaction of the outpoint and receive a notification for
	// it.
	if _, err = miner2.Client.SendRawTransaction(spendTx, true); err != nil {
		t.Fatalf("unable to broadcast spend tx: %v", err)
	}
	if err := chainntnfs.WaitForMempoolTx(miner, spendTxHash); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}
	if _, err := miner.Client.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}
	_, spendHeight, err = miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve current height")

	select {
	case spendDetails := <-spendIntent.Spend:
		checkNotificationFields(
			spendDetails, outpoint, spendTxHash, spendHeight, t,
		)
	case <-time.After(5 * time.Second):
		t.Fatal("expected spend notification to be dispatched")
	}
}

// testCatchUpClientOnMissedBlocks tests the case of multiple registered client
// receiving historical block epoch notifications due to their best known block
// being out of date.
func testCatchUpClientOnMissedBlocks(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, t *testing.T) {

	const numBlocks = 10
	const numClients = 5
	var wg sync.WaitGroup

	outdatedHash, outdatedHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve current height")

	// This function is used by UnsafeStart to ensure all notifications
	// are fully drained before clients register for notifications.
	generateBlocks := func() error {
		_, err = miner.Client.Generate(numBlocks)
		return err
	}

	// We want to ensure that when a client registers for block notifications,
	// the notifier's best block is at the tip of the chain. If it isn't, the
	// client may not receive all historical notifications.
	bestHeight := outdatedHeight + numBlocks
	err = notifier.UnsafeStart(bestHeight, nil, bestHeight, generateBlocks)
	require.NoError(t, err, "unable to unsafe start the notifier")
	defer notifier.Stop()

	// Create numClients clients whose best known block is 10 blocks behind
	// the tip of the chain. We expect each client to receive numBlocks
	// notifications, 1 for each block  they're behind.
	clients := make([]*chainntnfs.BlockEpochEvent, 0, numClients)
	outdatedBlock := &chainntnfs.BlockEpoch{
		Height: outdatedHeight, Hash: outdatedHash,
	}
	for i := 0; i < numClients; i++ {
		epochClient, err := notifier.RegisterBlockEpochNtfn(outdatedBlock)
		if err != nil {
			t.Fatalf("unable to register for epoch notification: %v", err)
		}
		clients = append(clients, epochClient)
	}
	for expectedHeight := outdatedHeight + 1; expectedHeight <=
		bestHeight; expectedHeight++ {

		for _, epochClient := range clients {
			select {
			case block := <-epochClient.Epochs:
				if block.Height != expectedHeight {
					t.Fatalf("received block of height: %d, "+
						"expected: %d", block.Height,
						expectedHeight)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("did not receive historical notification "+
					"for height %d", expectedHeight)
			}

		}
	}

	// Finally, ensure that an extra block notification wasn't received.
	anyExtras := make(chan struct{}, len(clients))
	for _, epochClient := range clients {
		wg.Add(1)
		go func(epochClient *chainntnfs.BlockEpochEvent) {
			defer wg.Done()
			select {
			case <-epochClient.Epochs:
				anyExtras <- struct{}{}
			case <-time.After(5 * time.Second):
			}
		}(epochClient)
	}

	wg.Wait()
	close(anyExtras)

	var extraCount int
	for range anyExtras {
		extraCount++
	}

	if extraCount > 0 {
		t.Fatalf("received %d unexpected block notification", extraCount)
	}
}

// testCatchUpOnMissedBlocks the case of multiple registered clients receiving
// historical block epoch notifications due to the notifier's best known block
// being out of date.
func testCatchUpOnMissedBlocks(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, t *testing.T) {

	const numBlocks = 10
	const numClients = 5
	var wg sync.WaitGroup

	_, bestHeight, err := miner.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	// This function is used by UnsafeStart to ensure all notifications
	// are fully drained before clients register for notifications.
	generateBlocks := func() error {
		_, err = miner.Client.Generate(numBlocks)
		return err
	}

	// Next, start the notifier with outdated best block information.
	err = notifier.UnsafeStart(
		bestHeight, nil, bestHeight+numBlocks, generateBlocks,
	)
	require.NoError(t, err, "unable to unsafe start the notifier")
	defer notifier.Stop()

	// Create numClients clients who will listen for block notifications.
	clients := make([]*chainntnfs.BlockEpochEvent, 0, numClients)
	for i := 0; i < numClients; i++ {
		epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
		if err != nil {
			t.Fatalf("unable to register for epoch notification: %v", err)
		}

		// Drain the notification dispatched upon registration as we're
		// not interested in it.
		select {
		case <-epochClient.Epochs:
		case <-time.After(5 * time.Second):
			t.Fatal("expected to receive epoch for current block " +
				"upon registration")
		}

		clients = append(clients, epochClient)
	}

	// Generate a single block to trigger the backlog of historical
	// notifications for the previously mined blocks.
	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// We expect each client to receive numBlocks + 1 notifications, 1 for
	// each block that the notifier has missed out on.
	for expectedHeight := bestHeight + 1; expectedHeight <=
		bestHeight+numBlocks+1; expectedHeight++ {

		for _, epochClient := range clients {
			select {
			case block := <-epochClient.Epochs:
				if block.Height != expectedHeight {
					t.Fatalf("received block of height: %d, "+
						"expected: %d", block.Height,
						expectedHeight)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("did not receive historical notification "+
					"for height %d", expectedHeight)
			}
		}
	}

	// Finally, ensure that an extra block notification wasn't received.
	anyExtras := make(chan struct{}, len(clients))
	for _, epochClient := range clients {
		wg.Add(1)
		go func(epochClient *chainntnfs.BlockEpochEvent) {
			defer wg.Done()
			select {
			case <-epochClient.Epochs:
				anyExtras <- struct{}{}
			case <-time.After(5 * time.Second):
			}
		}(epochClient)
	}

	wg.Wait()
	close(anyExtras)

	var extraCount int
	for range anyExtras {
		extraCount++
	}

	if extraCount > 0 {
		t.Fatalf("received %d unexpected block notification", extraCount)
	}
}

// testCatchUpOnMissedBlocks tests that a client will still receive all valid
// block notifications in the case where a notifier's best block has been reorged
// out of the chain.
func testCatchUpOnMissedBlocksWithReorg(miner1 *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, t *testing.T) {

	// If this is the neutrino notifier, then we'll skip this test for now
	// as we're missing functionality required to ensure the test passes
	// reliably.
	if _, ok := notifier.(*neutrinonotify.NeutrinoNotifier); ok {
		t.Skip("skipping re-org test for neutrino")
	}

	const numBlocks = 10
	const numClients = 5
	var wg sync.WaitGroup

	// Set up a new miner that we can use to cause a reorg.
	miner2 := unittest.NewMiner(
		t, unittest.NetParams, []string{"--txindex"}, false, 0,
	)

	// We start by connecting the new miner to our original miner,
	// such that it will sync to our original chain.
	if err := rpctest.ConnectNode(miner1, miner2); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice := []*rpctest.Harness{miner1, miner2}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// The two should be on the same blockheight.
	_, nodeHeight1, err := miner1.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, nodeHeight2, err := miner2.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if nodeHeight1 != nodeHeight2 {
		t.Fatalf("expected both miners to be on the same height: %v vs %v",
			nodeHeight1, nodeHeight2)
	}

	// We disconnect the two nodes, such that we can start mining on them
	// individually without the other one learning about the new blocks.
	err = miner1.Client.AddNode(miner2.P2PAddress(), rpcclient.ANRemove)
	require.NoError(t, err, "unable to remove node")

	// Now mine on each chain separately
	blocks, err := miner1.Client.Generate(numBlocks)
	require.NoError(t, err, "unable to generate single block")

	// We generate an extra block on miner 2's chain to ensure it is the
	// longer chain.
	_, err = miner2.Client.Generate(numBlocks + 1)
	require.NoError(t, err, "unable to generate single block")

	// Sync the two chains to ensure they will sync to miner2's chain.
	if err := rpctest.ConnectNode(miner1, miner2); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice = []*rpctest.Harness{miner1, miner2}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// The two should be on the same block hash.
	timeout := time.After(10 * time.Second)
	for {
		nodeHash1, _, err := miner1.Client.GetBestBlock()
		if err != nil {
			t.Fatalf("unable to get current block hash: %v", err)
		}

		nodeHash2, _, err := miner2.Client.GetBestBlock()
		if err != nil {
			t.Fatalf("unable to get current block hash: %v", err)
		}

		if *nodeHash1 == *nodeHash2 {
			break
		}
		select {
		case <-timeout:
			t.Fatalf("Unable to sync two chains")
		case <-time.After(50 * time.Millisecond):
			continue
		}
	}

	// Next, start the notifier with outdated best block information.
	// We set the notifier's best block to be the last block mined on the
	// shorter chain, to test that the notifier correctly rewinds to
	// the common ancestor between the two chains.
	syncHeight := nodeHeight1 + numBlocks + 1
	err = notifier.UnsafeStart(
		nodeHeight1+numBlocks, blocks[numBlocks-1], syncHeight, nil,
	)
	require.NoError(t, err, "Unable to unsafe start the notifier")
	defer notifier.Stop()

	// Create numClients clients who will listen for block notifications.
	clients := make([]*chainntnfs.BlockEpochEvent, 0, numClients)
	for i := 0; i < numClients; i++ {
		epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
		if err != nil {
			t.Fatalf("unable to register for epoch notification: %v", err)
		}

		// Drain the notification dispatched upon registration as we're
		// not interested in it.
		select {
		case <-epochClient.Epochs:
		case <-time.After(5 * time.Second):
			t.Fatal("expected to receive epoch for current block " +
				"upon registration")
		}

		clients = append(clients, epochClient)
	}

	// Generate a single block, which should trigger the notifier to rewind
	// to the common ancestor and dispatch notifications from there.
	_, err = miner2.Client.Generate(1)
	require.NoError(t, err, "unable to generate single block")

	// If the chain backend to the notifier stores information about reorged
	// blocks, the notifier is able to rewind the chain to the common
	// ancestor between the chain tip and its outdated best known block.
	// In this case, the client is expected to receive numBlocks + 2
	// notifications, 1 for each block the notifier has missed out on from
	// the longer chain.
	//
	// If the chain backend does not store information about reorged blocks,
	// the notifier has no way of knowing where to rewind to and therefore
	// the client is only expected to receive notifications for blocks
	// whose height is greater than the notifier's best known height: 2
	// notifications, in this case.
	var startingHeight int32
	switch notifier.(type) {
	case *neutrinonotify.NeutrinoNotifier:
		startingHeight = nodeHeight1 + numBlocks + 1
	default:
		startingHeight = nodeHeight1 + 1
	}

	for expectedHeight := startingHeight; expectedHeight <=
		nodeHeight1+numBlocks+2; expectedHeight++ {

		for _, epochClient := range clients {
			select {
			case block := <-epochClient.Epochs:
				if block.Height != expectedHeight {
					t.Fatalf("received block of height: %d, "+
						"expected: %d", block.Height,
						expectedHeight)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("did not receive historical notification "+
					"for height %d", expectedHeight)
			}
		}
	}

	// Finally, ensure that an extra block notification wasn't received.
	anyExtras := make(chan struct{}, len(clients))
	for _, epochClient := range clients {
		wg.Add(1)
		go func(epochClient *chainntnfs.BlockEpochEvent) {
			defer wg.Done()
			select {
			case <-epochClient.Epochs:
				anyExtras <- struct{}{}
			case <-time.After(5 * time.Second):
			}
		}(epochClient)
	}

	wg.Wait()
	close(anyExtras)

	var extraCount int
	for range anyExtras {
		extraCount++
	}

	if extraCount > 0 {
		t.Fatalf("received %d unexpected block notification", extraCount)
	}
}

// testIncludeBlockAsymmetry tests that if the same output is registered for a
// notification by two callers, one is able to get a notification with the
// block and the other one without it.
func testIncludeBlockAsymmetry(miner *rpctest.Harness,
	notifier chainntnfs.TestChainNotifier, scriptDispatch bool,
	t *testing.T) {

	// We'll start by creating a new test transaction, waiting long enough
	// for it to get into the mempool.
	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create test tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}

	_, currentHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to get current height")

	var (
		confIntentNoBlock *chainntnfs.ConfirmationEvent
		confIntentBlock   *chainntnfs.ConfirmationEvent

		numConfsLong  = uint32(6)
		numConfsShort = uint32(1)
	)
	dispatchClients := func() {
		dispatchTxid := txid
		if scriptDispatch {
			dispatchTxid = nil
		}

		confIntentNoBlock, err = notifier.RegisterConfirmationsNtfn(
			dispatchTxid, pkScript, numConfsLong,
			uint32(currentHeight),
		)
		require.NoError(t, err)

		confIntentBlock, err = notifier.RegisterConfirmationsNtfn(
			dispatchTxid, pkScript, numConfsShort,
			uint32(currentHeight), chainntnfs.WithIncludeBlock(),
		)
		require.NoError(t, err)
	}
	assertNtfns := func() {
		// Make sure the long confirmation client receives the
		// notification but not the block.
		confNtfnNoBlock, err := lnutils.RecvOrTimeout(
			confIntentNoBlock.Confirmed, time.Second*5,
		)
		require.NoError(t, err)
		require.Nil(t, (*confNtfnNoBlock).Block, "block not included")

		// And the short confirmation client receives the notification
		// and the block.
		confNtfnBlock, err := lnutils.RecvOrTimeout(
			confIntentBlock.Confirmed, time.Second*5,
		)
		require.NoError(t, err)
		require.NotNil(t, (*confNtfnBlock).Block, "block included")
	}

	// First, we start off by registering two clients for the same txid and
	// pkScript. One of them will require 6 confirmations but not request
	// the block, the other will just require a single confirmation and
	// request the block.
	dispatchClients()

	// Next, we'll generate a few blocks, which should confirm the
	// transaction created above and trigger the notifications, as it should
	// be confirmed enough for both clients.
	_, err = miner.Client.Generate(6)
	require.NoError(t, err, "unable to generate single block")

	// Make sure we got the notifications we expected.
	assertNtfns()

	// Now dispatch the same clients again, which should hit the same
	// conditions as above and use the cached rescan details.
	dispatchClients()

	// And again, the notifications should be triggered as expected.
	assertNtfns()
}

type txNtfnTestCase struct {
	name string
	test func(node *rpctest.Harness, notifier chainntnfs.TestChainNotifier,
		scriptDispatch bool, t *testing.T)
}

type blockNtfnTestCase struct {
	name string
	test func(node *rpctest.Harness, notifier chainntnfs.TestChainNotifier,
		t *testing.T)
}

type blockCatchupTestCase struct {
	name string
	test func(node *rpctest.Harness, notifier chainntnfs.TestChainNotifier,
		t *testing.T)
}

var txNtfnTests = []txNtfnTestCase{
	{
		name: "single conf ntfn",
		test: testSingleConfirmationNotification,
	},
	{
		name: "multi conf ntfn",
		test: testMultiConfirmationNotification,
	},
	{
		name: "batch conf ntfn",
		test: testBatchConfirmationNotification,
	},
	{
		name: "multi client conf",
		test: testMultiClientConfirmationNotification,
	},
	{
		name: "lazy ntfn consumer",
		test: testLazyNtfnConsumer,
	},
	{
		name: "historical conf dispatch",
		test: testTxConfirmedBeforeNtfnRegistration,
	},
	{
		name: "reorg conf",
		test: testReorgConf,
	},
	{
		name: "spend ntfn",
		test: testSpendNotification,
	},
	{
		name: "historical spend dispatch",
		test: testSpendBeforeNtfnRegistration,
	},
	{
		name: "reorg spend",
		test: testReorgSpend,
	},
	{
		name: "cancel spend ntfn",
		test: testCancelSpendNtfn,
	},
	{
		name: "test include block asymmetry",
		test: testIncludeBlockAsymmetry,
	},
}

var blockNtfnTests = []blockNtfnTestCase{
	{
		name: "block epoch",
		test: testBlockEpochNotification,
	},
	{
		name: "cancel epoch ntfn",
		test: testCancelEpochNtfn,
	},
}

var blockCatchupTests = []blockCatchupTestCase{
	{
		name: "catch up client on historical block epoch ntfns",
		test: testCatchUpClientOnMissedBlocks,
	},
	{
		name: "test catch up on missed blocks",
		test: testCatchUpOnMissedBlocks,
	},
	{
		name: "test catch up on missed blocks w/ reorged best block",
		test: testCatchUpOnMissedBlocksWithReorg,
	},
}

// TestInterfaces tests all registered interfaces with a unified set of tests
// which exercise each of the required methods found within the ChainNotifier
// interface.
//
// NOTE: In the future, when additional implementations of the ChainNotifier
// interface have been implemented, in order to ensure the new concrete
// implementation is automatically tested, two steps must be undertaken. First,
// one needs add a "non-captured" (_) import from the new sub-package. This
// import should trigger an init() method within the package which registers
// the interface. Second, an additional case in the switch within the main loop
// below needs to be added which properly initializes the interface.
func TestInterfaces(t *testing.T, targetBackEnd string) {
	// Initialize the harness around a btcd node which will serve as our
	// dedicated miner to generate blocks, cause re-orgs, etc. We'll set up
	// this node with a chain length of 125, so we have plenty of BTC to
	// play around with.
	miner := unittest.NewMiner(t, unittest.NetParams, nil, true, 25)

	p2pAddr := miner.P2PAddress()

	log.Printf("Running %v ChainNotifier interface tests",
		2*len(txNtfnTests)+len(blockNtfnTests)+len(blockCatchupTests))

	for _, notifierDriver := range chainntnfs.RegisteredNotifiers() {
		notifierType := notifierDriver.NotifierType
		if notifierType != targetBackEnd {
			continue
		}

		// Initialize a height hint cache for each notifier.
		tempDir := t.TempDir()
		db := channeldb.OpenForTesting(t, tempDir)

		testCfg := channeldb.CacheConfig{
			QueryDisable: false,
		}
		hintCache, err := channeldb.NewHeightHintCache(
			testCfg, db.Backend,
		)
		if err != nil {
			t.Fatalf("unable to create height hint cache: %v", err)
		}

		blockCache := blockcache.NewBlockCache(10000)

		var (
			newNotifier func() (chainntnfs.TestChainNotifier, error)
		)

		switch notifierType {
		case "bitcoind":
			var bitcoindConn *chain.BitcoindConn
			bitcoindConn = unittest.NewBitcoindBackend(
				t, unittest.NetParams, miner, true, false,
			)
			newNotifier = func() (chainntnfs.TestChainNotifier, error) {
				return bitcoindnotify.New(
					bitcoindConn, unittest.NetParams,
					hintCache, hintCache, blockCache,
				), nil
			}

		case "bitcoind-rpc-polling":
			var bitcoindConn *chain.BitcoindConn
			bitcoindConn = unittest.NewBitcoindBackend(
				t, unittest.NetParams, miner, true, true,
			)
			newNotifier = func() (chainntnfs.TestChainNotifier, error) {
				return bitcoindnotify.New(
					bitcoindConn, unittest.NetParams,
					hintCache, hintCache, blockCache,
				), nil
			}

		case "btcd":
			rpcConfig := miner.RPCConfig()
			newNotifier = func() (chainntnfs.TestChainNotifier, error) {
				return btcdnotify.New(
					&rpcConfig, unittest.NetParams,
					hintCache, hintCache, blockCache,
				)
			}

		case "neutrino":
			var spvNode *neutrino.ChainService
			spvNode = unittest.NewNeutrinoBackend(
				t, unittest.NetParams, p2pAddr,
			)
			newNotifier = func() (chainntnfs.TestChainNotifier, error) {
				return neutrinonotify.New(
					spvNode, hintCache, hintCache,
					blockCache,
				), nil
			}
		}

		log.Printf("Running ChainNotifier interface tests for: %v",
			notifierType)

		notifier, err := newNotifier()
		if err != nil {
			t.Fatalf("unable to create %v notifier: %v",
				notifierType, err)
		}
		if err := notifier.Start(); err != nil {
			t.Fatalf("unable to start notifier %v: %v",
				notifierType, err)
		}

		for _, txNtfnTest := range txNtfnTests {
			for _, scriptDispatch := range []bool{false, true} {
				testName := fmt.Sprintf("%v %v", notifierType,
					txNtfnTest.name)
				if scriptDispatch {
					testName += " with script dispatch"
				}
				success := t.Run(testName, func(t *testing.T) {
					txNtfnTest.test(
						miner, notifier, scriptDispatch,
						t,
					)
				})
				if !success {
					break
				}
			}
		}

		for _, blockNtfnTest := range blockNtfnTests {
			testName := fmt.Sprintf("%v %v", notifierType,
				blockNtfnTest.name)
			success := t.Run(testName, func(t *testing.T) {
				blockNtfnTest.test(miner, notifier, t)
			})
			if !success {
				break
			}
		}

		notifier.Stop()

		// Run catchup tests separately since they require restarting
		// the notifier every time.
		for _, blockCatchupTest := range blockCatchupTests {
			notifier, err = newNotifier()
			if err != nil {
				t.Fatalf("unable to create %v notifier: %v",
					notifierType, err)
			}

			testName := fmt.Sprintf("%v %v", notifierType,
				blockCatchupTest.name)

			success := t.Run(testName, func(t *testing.T) {
				blockCatchupTest.test(miner, notifier, t)
			})
			if !success {
				break
			}
		}
	}
}
