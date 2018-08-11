package chainntnfs_test

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

var zeroHash chainhash.Hash

// TestTxConfFutureDispatch tests that the TxConfNotifier dispatches
// registered notifications when the transaction confirms after registration.
func TestTxConfFutureDispatch(t *testing.T) {
	t.Parallel()

	const (
		tx1NumConfs uint32 = 1
		tx2NumConfs uint32 = 2
	)

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
		tx3 = wire.MsgTx{Version: 3}
	)

	txConfNotifier := chainntnfs.NewTxConfNotifier(10, 100)

	// Create the test transactions and register them with the
	// TxConfNotifier before including them in a block to receive future
	// notifications.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if err := txConfNotifier.Register(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if err := txConfNotifier.Register(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// We should not receive any notifications from both transactions
	// since they have not been included in a block yet.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	select {
	case <-ntfn2.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx2")
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	// Include the transactions in a block and add it to the TxConfNotifier.
	// This should confirm tx1, but not tx2.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1, &tx2, &tx3},
	})

	err := txConfNotifier.ConnectTip(
		block1.Hash(), 11, block1.Transactions(),
	)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// We should only receive one update for tx1 since it only requires
	// one confirmation and it already met it.
	select {
	case numConfsLeft := <-ntfn1.Event.Updates:
		const expected = 0
		if numConfsLeft != expected {
			t.Fatalf("Received incorrect confirmation update: tx1 "+
				"expected %d confirmations left, got %d",
				expected, numConfsLeft)
		}
	default:
		t.Fatal("Expected confirmation update for tx1")
	}

	// A confirmation notification for this tranaction should be dispatched,
	// as it only required one confirmation.
	select {
	case txConf := <-ntfn1.Event.Confirmed:
		expectedConf := chainntnfs.TxConfirmation{
			BlockHash:   block1.Hash(),
			BlockHeight: 11,
			TxIndex:     0,
		}
		assertEqualTxConf(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	// We should only receive one update for tx2 since it only has one
	// confirmation so far and it requires two.
	select {
	case numConfsLeft := <-ntfn2.Event.Updates:
		const expected = 1
		if numConfsLeft != expected {
			t.Fatalf("Received incorrect confirmation update: tx2 "+
				"expected %d confirmations left, got %d",
				expected, numConfsLeft)
		}
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should not be dispatched yet, as
	// it requires one more confirmation.
	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	// Create a new block and add it to the TxConfNotifier at the next
	// height. This should confirm tx2.
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx3},
	})

	err = txConfNotifier.ConnectTip(block2.Hash(), 12, block2.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// We should not receive any event notifications for tx1 since it has
	// already been confirmed.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	// We should only receive one update since the last at the new height,
	// indicating how many confirmations are still left.
	select {
	case numConfsLeft := <-ntfn2.Event.Updates:
		const expected = 0
		if numConfsLeft != expected {
			t.Fatalf("Received incorrect confirmation update: tx2 "+
				"expected %d confirmations left, got %d",
				expected, numConfsLeft)
		}
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should be dispatched, since it
	// now meets its required number of confirmations.
	select {
	case txConf := <-ntfn2.Event.Confirmed:
		expectedConf := chainntnfs.TxConfirmation{
			BlockHash:   block1.Hash(),
			BlockHeight: 11,
			TxIndex:     1,
		}
		assertEqualTxConf(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx2")
	}
}

// TestTxConfHistoricalDispatch tests that the TxConfNotifier dispatches
// registered notifications when the transaction is confirmed before
// registration.
func TestTxConfHistoricalDispatch(t *testing.T) {
	t.Parallel()

	const (
		tx1NumConfs uint32 = 1
		tx2NumConfs uint32 = 3
	)

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
		tx3 = wire.MsgTx{Version: 3}
	)

	txConfNotifier := chainntnfs.NewTxConfNotifier(10, 100)

	// Create the test transactions at a height before the TxConfNotifier's
	// starting height so that they are confirmed once registering them.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		ConfID:           0,
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if err := txConfNotifier.Register(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		ConfID:           1,
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if err := txConfNotifier.Register(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Update tx1 with its confirmation details. We should only receive one
	// update since it only requires one confirmation and it already met it.
	txConf1 := chainntnfs.TxConfirmation{
		BlockHash:   &zeroHash,
		BlockHeight: 9,
		TxIndex:     1,
	}
	err := txConfNotifier.UpdateConfDetails(tx1Hash, ntfn1.ConfID, &txConf1)
	if err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}
	select {
	case numConfsLeft := <-ntfn1.Event.Updates:
		const expected = 0
		if numConfsLeft != expected {
			t.Fatalf("Received incorrect confirmation update: tx1 "+
				"expected %d confirmations left, got %d",
				expected, numConfsLeft)
		}
	default:
		t.Fatal("Expected confirmation update for tx1")
	}

	// A confirmation notification for tx1 should also be dispatched.
	select {
	case txConf := <-ntfn1.Event.Confirmed:
		assertEqualTxConf(t, txConf, &txConf1)
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	// Update tx2 with its confirmation details. This should not trigger a
	// confirmation notification since it hasn't reached its required number
	// of confirmations, but we should receive a confirmation update
	// indicating how many confirmation are left.
	txConf2 := chainntnfs.TxConfirmation{
		BlockHash:   &zeroHash,
		BlockHeight: 9,
		TxIndex:     2,
	}
	err = txConfNotifier.UpdateConfDetails(tx2Hash, ntfn2.ConfID, &txConf2)
	if err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}
	select {
	case numConfsLeft := <-ntfn2.Event.Updates:
		const expected = 1
		if numConfsLeft != expected {
			t.Fatalf("Received incorrect confirmation update: tx2 "+
				"expected %d confirmations left, got %d",
				expected, numConfsLeft)
		}
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	// Create a new block and add it to the TxConfNotifier at the next
	// height. This should confirm tx2.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx3},
	})

	err = txConfNotifier.ConnectTip(block.Hash(), 11, block.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// We should not receive any event notifications for tx1 since it has
	// already been confirmed.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	// We should only receive one update for tx2 since the last one,
	// indicating how many confirmations are still left.
	select {
	case numConfsLeft := <-ntfn2.Event.Updates:
		const expected = 0
		if numConfsLeft != expected {
			t.Fatalf("Received incorrect confirmation update: tx2 "+
				"expected %d confirmations left, got %d",
				expected, numConfsLeft)
		}
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case txConf := <-ntfn2.Event.Confirmed:
		assertEqualTxConf(t, txConf, &txConf2)
	default:
		t.Fatalf("Expected confirmation for tx2")
	}
}

// TestTxConfChainReorg tests that TxConfNotifier dispatches Confirmed and
// NegativeConf notifications appropriately when there is a chain
// reorganization.
func TestTxConfChainReorg(t *testing.T) {
	t.Parallel()

	const (
		tx1NumConfs uint32 = 2
		tx2NumConfs uint32 = 1
		tx3NumConfs uint32 = 2
	)

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
		tx3 = wire.MsgTx{Version: 3}
	)

	txConfNotifier := chainntnfs.NewTxConfNotifier(7, 100)

	// Tx 1 will be confirmed in block 9 and requires 2 confs.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if err := txConfNotifier.Register(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Tx 2 will be confirmed in block 10 and requires 1 conf.
	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if err := txConfNotifier.Register(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Tx 3 will be confirmed in block 10 and requires 2 confs.
	tx3Hash := tx3.TxHash()
	ntfn3 := chainntnfs.ConfNtfn{
		TxID:             &tx3Hash,
		NumConfirmations: tx3NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx3NumConfs),
	}
	if err := txConfNotifier.Register(&ntfn3); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Sync chain to block 10. Txs 1 & 2 should be confirmed.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})
	err := txConfNotifier.ConnectTip(nil, 8, block1.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	err = txConfNotifier.ConnectTip(nil, 9, nil)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2, &tx3},
	})
	err = txConfNotifier.ConnectTip(nil, 10, block2.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// We should receive two updates for tx1 since it requires two
	// confirmations and it has already met them.
	for i := 0; i < 2; i++ {
		select {
		case <-ntfn1.Event.Updates:
		default:
			t.Fatal("Expected confirmation update for tx1")
		}
	}

	// A confirmation notification for tx1 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case <-ntfn1.Event.Confirmed:
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	// We should only receive one update for tx2 since it only requires
	// one confirmation and it already met it.
	select {
	case <-ntfn2.Event.Updates:
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case <-ntfn2.Event.Confirmed:
	default:
		t.Fatalf("Expected confirmation for tx2")
	}

	// We should only receive one update for tx3 since it only has one
	// confirmation so far and it requires two.
	select {
	case <-ntfn3.Event.Updates:
	default:
		t.Fatal("Expected confirmation update for tx3")
	}

	// A confirmation notification for tx3 should not be dispatched yet, as
	// it requires one more confirmation.
	select {
	case txConf := <-ntfn3.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx3: %v", txConf)
	default:
	}

	// The block that included tx2 and tx3 is disconnected and two next
	// blocks without them are connected.
	err = txConfNotifier.DisconnectTip(10)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	err = txConfNotifier.ConnectTip(nil, 10, nil)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	err = txConfNotifier.ConnectTip(nil, 11, nil)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	select {
	case reorgDepth := <-ntfn2.Event.NegativeConf:
		if reorgDepth != 1 {
			t.Fatalf("Incorrect value for negative conf notification: "+
				"expected %d, got %d", 1, reorgDepth)
		}
	default:
		t.Fatalf("Expected negative conf notification for tx1")
	}

	// We should not receive any event notifications from all of the
	// transactions because tx1 has already been confirmed and tx2 and tx3
	// have not been included in the chain since the reorg.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	select {
	case <-ntfn2.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx2")
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	select {
	case <-ntfn3.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx3")
	case txConf := <-ntfn3.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx3: %v", txConf)
	default:
	}

	// Now transactions 2 & 3 are re-included in a new block.
	block3 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2, &tx3},
	})
	block4 := btcutil.NewBlock(&wire.MsgBlock{})

	err = txConfNotifier.ConnectTip(block3.Hash(), 12, block3.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	err = txConfNotifier.ConnectTip(block4.Hash(), 13, block4.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// We should only receive one update for tx2 since it only requires
	// one confirmation and it already met it.
	select {
	case numConfsLeft := <-ntfn2.Event.Updates:
		const expected = 0
		if numConfsLeft != expected {
			t.Fatalf("Received incorrect confirmation update: tx2 "+
				"expected %d confirmations left, got %d",
				expected, numConfsLeft)
		}
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	// A confirmation notification for tx2 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case txConf := <-ntfn2.Event.Confirmed:
		expectedConf := chainntnfs.TxConfirmation{
			BlockHash:   block3.Hash(),
			BlockHeight: 12,
			TxIndex:     0,
		}
		assertEqualTxConf(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx2")
	}

	// We should receive two updates for tx3 since it requires two
	// confirmations and it has already met them.
	for i := uint32(1); i <= 2; i++ {
		select {
		case numConfsLeft := <-ntfn3.Event.Updates:
			expected := tx3NumConfs - i
			if numConfsLeft != expected {
				t.Fatalf("Received incorrect confirmation update: tx3 "+
					"expected %d confirmations left, got %d",
					expected, numConfsLeft)
			}
		default:
			t.Fatal("Expected confirmation update for tx2")
		}
	}

	// A confirmation notification for tx3 should be dispatched, as it met
	// its required number of confirmations.
	select {
	case txConf := <-ntfn3.Event.Confirmed:
		expectedConf := chainntnfs.TxConfirmation{
			BlockHash:   block3.Hash(),
			BlockHeight: 12,
			TxIndex:     1,
		}
		assertEqualTxConf(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx3")
	}
}

func TestTxConfTearDown(t *testing.T) {
	t.Parallel()

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
	)

	txConfNotifier := chainntnfs.NewTxConfNotifier(10, 100)

	// Create the test transactions and register them with the
	// TxConfNotifier to receive notifications.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: 1,
		Event:            chainntnfs.NewConfirmationEvent(1),
	}
	if err := txConfNotifier.Register(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: 2,
		Event:            chainntnfs.NewConfirmationEvent(2),
	}
	if err := txConfNotifier.Register(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Include the transactions in a block and add it to the TxConfNotifier.
	// This should confirm tx1, but not tx2.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1, &tx2},
	})

	err := txConfNotifier.ConnectTip(block.Hash(), 11, block.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// We do not care about the correctness of the notifications since they
	// are tested in other methods, but we'll still attempt to retrieve them
	// for the sake of not being able to later once the notification
	// channels are closed.
	select {
	case <-ntfn1.Event.Updates:
	default:
		t.Fatal("Expected confirmation update for tx1")
	}

	select {
	case <-ntfn1.Event.Confirmed:
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	select {
	case <-ntfn2.Event.Updates:
	default:
		t.Fatal("Expected confirmation update for tx2")
	}

	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	// The notification channels should be closed for notifications that
	// have not been dispatched yet, so we should not expect to receive any
	// more updates.
	txConfNotifier.TearDown()

	// tx1 should not receive any more updates because it has already been
	// confirmed and the TxConfNotifier has been shut down.
	select {
	case <-ntfn1.Event.Updates:
		t.Fatal("Received unexpected confirmation update for tx1")
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	// tx2 should not receive any more updates after the notifications
	// channels have been closed and the TxConfNotifier shut down.
	select {
	case _, more := <-ntfn2.Event.Updates:
		if more {
			t.Fatal("Expected closed Updates channel for tx2")
		}
	case _, more := <-ntfn2.Event.Confirmed:
		if more {
			t.Fatalf("Expected closed Confirmed channel for tx2")
		}
	default:
		t.Fatalf("Expected closed notification channels for tx2")
	}
}

func assertEqualTxConf(t *testing.T,
	actualConf, expectedConf *chainntnfs.TxConfirmation) {

	if actualConf.BlockHeight != expectedConf.BlockHeight {
		t.Fatalf("Incorrect block height in confirmation details: "+
			"expected %d, got %d",
			expectedConf.BlockHeight, actualConf.BlockHeight)
	}
	if !actualConf.BlockHash.IsEqual(expectedConf.BlockHash) {
		t.Fatalf("Incorrect block hash in confirmation details: "+
			"expected %d, got %d", expectedConf.BlockHash, actualConf.BlockHash)
	}
	if actualConf.TxIndex != expectedConf.TxIndex {
		t.Fatalf("Incorrect tx index in confirmation details: "+
			"expected %d, got %d", expectedConf.TxIndex, actualConf.TxIndex)
	}
}
