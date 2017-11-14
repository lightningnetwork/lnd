package chainntnfs_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var zeroHash chainhash.Hash

// TestTxConfFutureDispatch tests that the TxConfNotifier dispatches
// registered notifications when the transaction confirms after registration.
func TestTxConfFutureDispatch(t *testing.T) {
	t.Parallel()

	txConfNotifier := chainntnfs.NewTxConfNotifier(10, 100)

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
		tx3 = wire.MsgTx{Version: 3}
	)

	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: 1,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConfNotifier.Register(&ntfn1, nil)

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: 2,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConfNotifier.Register(&ntfn2, nil)

	select {
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1, &tx2, &tx3},
	})

	err := txConfNotifier.ConnectTip(block1.Hash(), 11, block1.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

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

	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx3},
	})

	err = txConfNotifier.ConnectTip(block2.Hash(), 12, block2.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	select {
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

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

	txConfNotifier := chainntnfs.NewTxConfNotifier(10, 100)

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
		tx3 = wire.MsgTx{Version: 3}
	)

	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: 1,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConf1 := chainntnfs.TxConfirmation{
		BlockHash:   &zeroHash,
		BlockHeight: 9,
		TxIndex:     1,
	}
	txConfNotifier.Register(&ntfn1, &txConf1)

	tx2Hash := tx2.TxHash()
	txConf2 := chainntnfs.TxConfirmation{
		BlockHash:   &zeroHash,
		BlockHeight: 9,
		TxIndex:     2,
	}
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: 3,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConfNotifier.Register(&ntfn2, &txConf2)

	select {
	case txConf := <-ntfn1.Event.Confirmed:
		assertEqualTxConf(t, txConf, &txConf1)
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx3},
	})

	err := txConfNotifier.ConnectTip(block.Hash(), 11, block.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	select {
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

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

	txConfNotifier := chainntnfs.NewTxConfNotifier(8, 100)

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
		tx3 = wire.MsgTx{Version: 3}
	)

	// Tx 1 will be confirmed in block 9 and requires 2 confs.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: 2,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConfNotifier.Register(&ntfn1, nil)

	// Tx 2 will be confirmed in block 10 and requires 1 conf.
	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: 1,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConfNotifier.Register(&ntfn2, nil)

	// Tx 3 will be confirmed in block 10 and requires 2 confs.
	tx3Hash := tx3.TxHash()
	ntfn3 := chainntnfs.ConfNtfn{
		TxID:             &tx3Hash,
		NumConfirmations: 2,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConfNotifier.Register(&ntfn3, nil)

	// Sync chain to block 10. Txs 1 & 2 should be confirmed.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})
	err := txConfNotifier.ConnectTip(nil, 9, block1.Transactions())
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

	select {
	case <-ntfn1.Event.Confirmed:
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	select {
	case <-ntfn2.Event.Confirmed:
	default:
		t.Fatalf("Expected confirmation for tx2")
	}

	select {
	case txConf := <-ntfn3.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx3: %v", txConf)
	default:
	}

	// Block that tx2 and tx3 were included in is disconnected and two next
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

	select {
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	select {
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

	// Both transactions should be newly confirmed.
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

	txConfNotifier := chainntnfs.NewTxConfNotifier(10, 100)

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
	)

	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: 1,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConfNotifier.Register(&ntfn1, nil)

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: 2,
		Event:            chainntnfs.NewConfirmationEvent(),
	}
	txConfNotifier.Register(&ntfn2, nil)

	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1, &tx2},
	})

	err := txConfNotifier.ConnectTip(block.Hash(), 11, block.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	select {
	case <-ntfn1.Event.Confirmed:
	default:
		t.Fatalf("Expected confirmation for tx1")
	}

	select {
	case txConf := <-ntfn2.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx2: %v", txConf)
	default:
	}

	// Confirmed channels should be closed for notifications that have not been
	// dispatched yet.
	txConfNotifier.TearDown()

	select {
	case txConf := <-ntfn1.Event.Confirmed:
		t.Fatalf("Received unexpected confirmation for tx1: %v", txConf)
	default:
	}

	select {
	case _, more := <-ntfn2.Event.Confirmed:
		if more {
			t.Fatalf("Expected channel close for tx2")
		}
	default:
		t.Fatalf("Expected channel close for tx2")
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
