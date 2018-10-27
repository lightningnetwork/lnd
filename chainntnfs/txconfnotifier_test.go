package chainntnfs_test

import (
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

var zeroHash chainhash.Hash

type mockHintCache struct {
	mu         sync.Mutex
	confHints  map[chainhash.Hash]uint32
	spendHints map[wire.OutPoint]uint32
}

var _ chainntnfs.SpendHintCache = (*mockHintCache)(nil)
var _ chainntnfs.ConfirmHintCache = (*mockHintCache)(nil)

func (c *mockHintCache) CommitSpendHint(heightHint uint32, ops ...wire.OutPoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, op := range ops {
		c.spendHints[op] = heightHint
	}

	return nil
}

func (c *mockHintCache) QuerySpendHint(op wire.OutPoint) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	hint, ok := c.spendHints[op]
	if !ok {
		return 0, chainntnfs.ErrSpendHintNotFound
	}

	return hint, nil
}

func (c *mockHintCache) PurgeSpendHint(ops ...wire.OutPoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, op := range ops {
		delete(c.spendHints, op)
	}

	return nil
}

func (c *mockHintCache) CommitConfirmHint(heightHint uint32, txids ...chainhash.Hash) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, txid := range txids {
		c.confHints[txid] = heightHint
	}

	return nil
}

func (c *mockHintCache) QueryConfirmHint(txid chainhash.Hash) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	hint, ok := c.confHints[txid]
	if !ok {
		return 0, chainntnfs.ErrConfirmHintNotFound
	}

	return hint, nil
}

func (c *mockHintCache) PurgeConfirmHint(txids ...chainhash.Hash) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, txid := range txids {
		delete(c.confHints, txid)
	}

	return nil
}

func newMockHintCache() *mockHintCache {
	return &mockHintCache{
		confHints:  make(map[chainhash.Hash]uint32),
		spendHints: make(map[wire.OutPoint]uint32),
	}
}

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

	hintCache := newMockHintCache()
	tcn := chainntnfs.NewTxConfNotifier(10, 100, hintCache)

	// Create the test transactions and register them with the
	// TxConfNotifier before including them in a block to receive future
	// notifications.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if _, err := tcn.Register(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if _, err := tcn.Register(&ntfn2); err != nil {
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

	err := tcn.ConnectTip(
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

	err = tcn.ConnectTip(block2.Hash(), 12, block2.Transactions())
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

	hintCache := newMockHintCache()
	tcn := chainntnfs.NewTxConfNotifier(10, 100, hintCache)

	// Create the test transactions at a height before the TxConfNotifier's
	// starting height so that they are confirmed once registering them.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		ConfID:           0,
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if _, err := tcn.Register(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		ConfID:           1,
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if _, err := tcn.Register(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Update tx1 with its confirmation details. We should only receive one
	// update since it only requires one confirmation and it already met it.
	txConf1 := chainntnfs.TxConfirmation{
		BlockHash:   &zeroHash,
		BlockHeight: 9,
		TxIndex:     1,
	}
	err := tcn.UpdateConfDetails(tx1Hash, &txConf1)
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
	err = tcn.UpdateConfDetails(tx2Hash, &txConf2)
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

	err = tcn.ConnectTip(block.Hash(), 11, block.Transactions())
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

	hintCache := newMockHintCache()
	tcn := chainntnfs.NewTxConfNotifier(7, 100, hintCache)

	// Tx 1 will be confirmed in block 9 and requires 2 confs.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if _, err := tcn.Register(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	if err := tcn.UpdateConfDetails(*ntfn1.TxID, nil); err != nil {
		t.Fatalf("unable to deliver conf details: %v", err)
	}

	// Tx 2 will be confirmed in block 10 and requires 1 conf.
	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if _, err := tcn.Register(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	if err := tcn.UpdateConfDetails(*ntfn2.TxID, nil); err != nil {
		t.Fatalf("unable to deliver conf details: %v", err)
	}

	// Tx 3 will be confirmed in block 10 and requires 2 confs.
	tx3Hash := tx3.TxHash()
	ntfn3 := chainntnfs.ConfNtfn{
		TxID:             &tx3Hash,
		NumConfirmations: tx3NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx3NumConfs),
	}
	if _, err := tcn.Register(&ntfn3); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	if err := tcn.UpdateConfDetails(*ntfn3.TxID, nil); err != nil {
		t.Fatalf("unable to deliver conf details: %v", err)
	}

	// Sync chain to block 10. Txs 1 & 2 should be confirmed.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})
	err := tcn.ConnectTip(nil, 8, block1.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	err = tcn.ConnectTip(nil, 9, nil)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2, &tx3},
	})
	err = tcn.ConnectTip(nil, 10, block2.Transactions())
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
	err = tcn.DisconnectTip(10)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	err = tcn.ConnectTip(nil, 10, nil)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	err = tcn.ConnectTip(nil, 11, nil)
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

	err = tcn.ConnectTip(block3.Hash(), 12, block3.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	err = tcn.ConnectTip(block4.Hash(), 13, block4.Transactions())
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

// TestTxConfHeightHintCache ensures that the height hints for transactions are
// kept track of correctly with each new block connected/disconnected. This test
// also asserts that the height hints are not updated until the simulated
// historical dispatches have returned, and we know the transactions aren't
// already in the chain.
func TestTxConfHeightHintCache(t *testing.T) {
	t.Parallel()

	const (
		startingHeight = 200
		txDummyHeight  = 201
		tx1Height      = 202
		tx2Height      = 203
	)

	// Initialize our TxConfNotifier instance backed by a height hint cache.
	hintCache := newMockHintCache()
	tcn := chainntnfs.NewTxConfNotifier(
		startingHeight, 100, hintCache,
	)

	// Create two test transactions and register them for notifications.
	tx1 := wire.MsgTx{Version: 1}
	tx1Hash := tx1.TxHash()
	ntfn1 := &chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: 1,
		Event:            chainntnfs.NewConfirmationEvent(1),
	}

	tx2 := wire.MsgTx{Version: 2}
	tx2Hash := tx2.TxHash()
	ntfn2 := &chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: 2,
		Event:            chainntnfs.NewConfirmationEvent(2),
	}

	if _, err := tcn.Register(ntfn1); err != nil {
		t.Fatalf("unable to register tx1: %v", err)
	}
	if _, err := tcn.Register(ntfn2); err != nil {
		t.Fatalf("unable to register tx2: %v", err)
	}

	// Both transactions should not have a height hint set, as Register
	// should not alter the cache state.
	_, err := hintCache.QueryConfirmHint(tx1Hash)
	if err != chainntnfs.ErrConfirmHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"want: %v, got %v",
			chainntnfs.ErrConfirmHintNotFound, err)
	}

	_, err = hintCache.QueryConfirmHint(tx2Hash)
	if err != chainntnfs.ErrConfirmHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"want: %v, got %v",
			chainntnfs.ErrConfirmHintNotFound, err)
	}

	// Create a new block that will include the dummy transaction and extend
	// the chain.
	txDummy := wire.MsgTx{Version: 3}
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&txDummy},
	})

	err = tcn.ConnectTip(
		block1.Hash(), txDummyHeight, block1.Transactions(),
	)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// Since UpdateConfDetails has not been called for either transaction,
	// the height hints should remain unchanged. This simulates blocks
	// confirming while the historical dispatch is processing the
	// registration.
	hint, err := hintCache.QueryConfirmHint(tx1Hash)
	if err != chainntnfs.ErrConfirmHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"want: %v, got %v",
			chainntnfs.ErrConfirmHintNotFound, err)
	}

	hint, err = hintCache.QueryConfirmHint(tx2Hash)
	if err != chainntnfs.ErrConfirmHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"want: %v, got %v",
			chainntnfs.ErrConfirmHintNotFound, err)
	}

	// Now, update the conf details reporting that the neither txn was found
	// in the historical dispatch.
	if err := tcn.UpdateConfDetails(tx1Hash, nil); err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}
	if err := tcn.UpdateConfDetails(tx2Hash, nil); err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}

	// We'll create another block that will include the first transaction
	// and extend the chain.
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})

	err = tcn.ConnectTip(
		block2.Hash(), tx1Height, block2.Transactions(),
	)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// Now that both notifications are waiting at tip for confirmations,
	// they should have their height hints updated to the latest block
	// height.
	hint, err = hintCache.QueryConfirmHint(tx1Hash)
	if err != nil {
		t.Fatalf("unable to query for hint: %v", err)
	}
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx1Height, hint)
	}

	hint, err = hintCache.QueryConfirmHint(tx2Hash)
	if err != nil {
		t.Fatalf("unable to query for hint: %v", err)
	}
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx2Height, hint)
	}

	// Next, we'll create another block that will include the second
	// transaction and extend the chain.
	block3 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2},
	})

	err = tcn.ConnectTip(
		block3.Hash(), tx2Height, block3.Transactions(),
	)
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	// The height hint for the first transaction should remain the same.
	hint, err = hintCache.QueryConfirmHint(tx1Hash)
	if err != nil {
		t.Fatalf("unable to query for hint: %v", err)
	}
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx1Height, hint)
	}

	// The height hint for the second transaction should now be updated to
	// reflect its confirmation.
	hint, err = hintCache.QueryConfirmHint(tx2Hash)
	if err != nil {
		t.Fatalf("unable to query for hint: %v", err)
	}
	if hint != tx2Height {
		t.Fatalf("expected hint %d, got %d",
			tx2Height, hint)
	}

	// Finally, we'll attempt do disconnect the last block in order to
	// simulate a chain reorg.
	if err := tcn.DisconnectTip(tx2Height); err != nil {
		t.Fatalf("Failed to disconnect block: %v", err)
	}

	// This should update the second transaction's height hint within the
	// cache to the previous height.
	hint, err = hintCache.QueryConfirmHint(tx2Hash)
	if err != nil {
		t.Fatalf("unable to query for hint: %v", err)
	}
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx1Height, hint)
	}

	// The first transaction's height hint should remain at the original
	// confirmation height.
	hint, err = hintCache.QueryConfirmHint(tx2Hash)
	if err != nil {
		t.Fatalf("unable to query for hint: %v", err)
	}
	if hint != tx1Height {
		t.Fatalf("expected hint %d, got %d",
			tx1Height, hint)
	}
}

func TestTxConfTearDown(t *testing.T) {
	t.Parallel()

	var (
		tx1 = wire.MsgTx{Version: 1}
		tx2 = wire.MsgTx{Version: 2}
	)

	hintCache := newMockHintCache()
	tcn := chainntnfs.NewTxConfNotifier(10, 100, hintCache)

	// Create the test transactions and register them with the
	// TxConfNotifier to receive notifications.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: 1,
		Event:            chainntnfs.NewConfirmationEvent(1),
	}
	if _, err := tcn.Register(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}
	if err := tcn.UpdateConfDetails(*ntfn1.TxID, nil); err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: 2,
		Event:            chainntnfs.NewConfirmationEvent(2),
	}
	if _, err := tcn.Register(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}
	if err := tcn.UpdateConfDetails(*ntfn2.TxID, nil); err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}

	// Include the transactions in a block and add it to the TxConfNotifier.
	// This should confirm tx1, but not tx2.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1, &tx2},
	})

	err := tcn.ConnectTip(block.Hash(), 11, block.Transactions())
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
	tcn.TearDown()

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
