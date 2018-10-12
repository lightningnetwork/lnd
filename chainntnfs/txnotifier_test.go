package chainntnfs_test

import (
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

var (
	zeroHash     chainhash.Hash
	zeroOutPoint wire.OutPoint
)

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

// TestTxNotifierFutureConfDispatch tests that the TxNotifier dispatches
// registered notifications when a transaction confirms after registration.
func TestTxNotifierFutureConfDispatch(t *testing.T) {
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
	n := chainntnfs.NewTxNotifier(10, 100, hintCache, hintCache)

	// Create the test transactions and register them with the TxNotifier
	// before including them in a block to receive future
	// notifications.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if _, err := n.RegisterConf(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if _, err := n.RegisterConf(&ntfn2); err != nil {
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

	// Include the transactions in a block and add it to the TxNotifier.
	// This should confirm tx1, but not tx2.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1, &tx2, &tx3},
	})

	err := n.ConnectTip(block1.Hash(), 11, block1.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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
		assertConfDetails(t, txConf, &expectedConf)
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

	// Create a new block and add it to the TxNotifier at the next height.
	// This should confirm tx2.
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx3},
	})

	err = n.ConnectTip(block2.Hash(), 12, block2.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(12); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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
		assertConfDetails(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx2")
	}
}

// TestTxNotifierHistoricalConfDispatch tests that the TxNotifier dispatches
// registered notifications when the transaction is confirmed before
// registration.
func TestTxNotifierHistoricalConfDispatch(t *testing.T) {
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
	n := chainntnfs.NewTxNotifier(10, 100, hintCache, hintCache)

	// Create the test transactions at a height before the TxNotifier's
	// starting height so that they are confirmed once registering them.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		ConfID:           0,
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if _, err := n.RegisterConf(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		ConfID:           1,
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if _, err := n.RegisterConf(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	// Update tx1 with its confirmation details. We should only receive one
	// update since it only requires one confirmation and it already met it.
	txConf1 := chainntnfs.TxConfirmation{
		BlockHash:   &zeroHash,
		BlockHeight: 9,
		TxIndex:     1,
	}
	err := n.UpdateConfDetails(tx1Hash, &txConf1)
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
		assertConfDetails(t, txConf, &txConf1)
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
	err = n.UpdateConfDetails(tx2Hash, &txConf2)
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

	// Create a new block and add it to the TxNotifier at the next height.
	// This should confirm tx2.
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx3},
	})

	err = n.ConnectTip(block.Hash(), 11, block.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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
		assertConfDetails(t, txConf, &txConf2)
	default:
		t.Fatalf("Expected confirmation for tx2")
	}
}

// TestTxNotifierFutureSpendDispatch tests that the TxNotifier dispatches
// registered notifications when an outpoint is spent after registration.
func TestTxNotifierFutureSpendDispatch(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(10, 100, hintCache, hintCache)

	// We'll start off by registering for a spend notification of an
	// outpoint.
	ntfn := &chainntnfs.SpendNtfn{
		OutPoint: zeroOutPoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	if _, err := n.RegisterSpend(ntfn); err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}

	// We should not receive a notification as the outpoint has not been
	// spent yet.
	select {
	case <-ntfn.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}

	// Construct the details of the spending transaction of the outpoint
	// above. We'll include it in the next block, which should trigger a
	// spend notification.
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{PreviousOutPoint: zeroOutPoint})
	spendTxHash := spendTx.TxHash()
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})
	err := n.ConnectTip(block.Hash(), 11, block.Transactions())
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	expectedSpendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &ntfn.OutPoint,
		SpenderTxHash:     &spendTxHash,
		SpendingTx:        spendTx,
		SpenderInputIndex: 0,
		SpendingHeight:    11,
	}

	// Ensure that the details of the notification match as expected.
	select {
	case spendDetails := <-ntfn.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails)
	default:
		t.Fatal("expected to receive spend details")
	}

	// Finally, we'll ensure that if the spending transaction has also been
	// spent, then we don't receive another spend notification.
	prevOut := wire.OutPoint{Hash: spendTxHash, Index: 0}
	spendOfSpend := wire.NewMsgTx(2)
	spendOfSpend.AddTxIn(&wire.TxIn{PreviousOutPoint: prevOut})
	block = btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendOfSpend},
	})
	err = n.ConnectTip(block.Hash(), 12, block.Transactions())
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(12); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	select {
	case <-ntfn.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}
}

// TestTxNotifierHistoricalSpendDispatch tests that the TxNotifier dispatches
// registered notifications when an outpoint is spent before registration.
func TestTxNotifierHistoricalSpendDispatch(t *testing.T) {
	t.Parallel()

	const startingHeight = 10

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

	// We'll start by constructing the spending details of the outpoint
	// below.
	spentOutpoint := zeroOutPoint
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{PreviousOutPoint: zeroOutPoint})
	spendTxHash := spendTx.TxHash()

	expectedSpendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &spentOutpoint,
		SpenderTxHash:     &spendTxHash,
		SpendingTx:        spendTx,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight - 1,
	}

	// We'll register for a spend notification of the outpoint and ensure
	// that a notification isn't dispatched.
	ntfn := &chainntnfs.SpendNtfn{
		OutPoint: spentOutpoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	if _, err := n.RegisterSpend(ntfn); err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}

	select {
	case <-ntfn.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}

	// Because we're interested in testing the case of a historical spend,
	// we'll hand off the spending details of the outpoint to the notifier
	// as it is not possible for it to view historical events in the chain.
	// By doing this, we replicate the functionality of the ChainNotifier.
	err := n.UpdateSpendDetails(ntfn.OutPoint, expectedSpendDetails)
	if err != nil {
		t.Fatalf("unable to update spend details: %v", err)
	}

	// Now that we have the spending details, we should receive a spend
	// notification. We'll ensure that the details match as intended.
	select {
	case spendDetails := <-ntfn.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails)
	default:
		t.Fatalf("expected to receive spend details")
	}

	// Finally, we'll ensure that if the spending transaction has also been
	// spent, then we don't receive another spend notification.
	prevOut := wire.OutPoint{Hash: spendTxHash, Index: 0}
	spendOfSpend := wire.NewMsgTx(2)
	spendOfSpend.AddTxIn(&wire.TxIn{PreviousOutPoint: prevOut})
	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendOfSpend},
	})
	err = n.ConnectTip(block.Hash(), startingHeight+1, block.Transactions())
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(startingHeight + 1); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	select {
	case <-ntfn.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}
}

// TestTxNotifierMultipleHistoricalRescans ensures that we don't attempt to
// request multiple historical confirmation rescans per transactions.
func TestTxNotifierMultipleHistoricalConfRescans(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

	// The first registration for a transaction in the notifier should
	// request a historical confirmation rescan as it does not have a
	// historical view of the chain.
	confNtfn1 := &chainntnfs.ConfNtfn{
		ConfID: 0,
		TxID:   &zeroHash,
		Event:  chainntnfs.NewConfirmationEvent(1),
	}
	historicalConfDispatch1, err := n.RegisterConf(confNtfn1)
	if err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}
	if historicalConfDispatch1 == nil {
		t.Fatal("expected to receive historical dispatch request")
	}

	// We'll register another confirmation notification for the same
	// transaction. This should not request a historical confirmation rescan
	// since the first one is still pending.
	confNtfn2 := &chainntnfs.ConfNtfn{
		ConfID: 1,
		TxID:   &zeroHash,
		Event:  chainntnfs.NewConfirmationEvent(1),
	}
	historicalConfDispatch2, err := n.RegisterConf(confNtfn2)
	if err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}
	if historicalConfDispatch2 != nil {
		t.Fatal("received unexpected historical rescan request")
	}

	// Finally, we'll mark the ongoing historical rescan as complete and
	// register another notification. We should also expect not to see a
	// historical rescan request since the confirmation details should be
	// cached.
	confDetails := &chainntnfs.TxConfirmation{
		BlockHeight: startingHeight - 1,
	}
	if err := n.UpdateConfDetails(*confNtfn2.TxID, confDetails); err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}

	confNtfn3 := &chainntnfs.ConfNtfn{
		ConfID: 2,
		TxID:   &zeroHash,
		Event:  chainntnfs.NewConfirmationEvent(1),
	}
	historicalConfDispatch3, err := n.RegisterConf(confNtfn3)
	if err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}
	if historicalConfDispatch3 != nil {
		t.Fatal("received unexpected historical rescan request")
	}
}

// TestTxNotifierMultipleHistoricalRescans ensures that we don't attempt to
// request multiple historical spend rescans per outpoints.
func TestTxNotifierMultipleHistoricalSpendRescans(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

	// The first registration for an outpoint in the notifier should request
	// a historical spend rescan as it does not have a historical view of
	// the chain.
	ntfn1 := &chainntnfs.SpendNtfn{
		SpendID:  0,
		OutPoint: zeroOutPoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	historicalDispatch1, err := n.RegisterSpend(ntfn1)
	if err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}
	if historicalDispatch1 == nil {
		t.Fatal("expected to receive historical dispatch request")
	}

	// We'll register another spend notification for the same outpoint. This
	// should not request a historical spend rescan since the first one is
	// still pending.
	ntfn2 := &chainntnfs.SpendNtfn{
		SpendID:  1,
		OutPoint: zeroOutPoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	historicalDispatch2, err := n.RegisterSpend(ntfn2)
	if err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}
	if historicalDispatch2 != nil {
		t.Fatal("received unexpected historical rescan request")
	}

	// Finally, we'll mark the ongoing historical rescan as complete and
	// register another notification. We should also expect not to see a
	// historical rescan request since the confirmation details should be
	// cached.
	spendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &ntfn2.OutPoint,
		SpenderTxHash:     &zeroHash,
		SpendingTx:        wire.NewMsgTx(2),
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight - 1,
	}
	err = n.UpdateSpendDetails(ntfn2.OutPoint, spendDetails)
	if err != nil {
		t.Fatalf("unable to update spend details: %v", err)
	}

	ntfn3 := &chainntnfs.SpendNtfn{
		SpendID:  2,
		OutPoint: zeroOutPoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	historicalDispatch3, err := n.RegisterSpend(ntfn3)
	if err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}
	if historicalDispatch3 != nil {
		t.Fatal("received unexpected historical rescan request")
	}
}

// TestTxNotifierMultipleHistoricalNtfns ensures that the TxNotifier will only
// request one rescan for a transaction/outpoint when having multiple client
// registrations. Once the rescan has completed and retrieved the
// confirmation/spend details, a notification should be dispatched to _all_
// clients.
func TestTxNotifierMultipleHistoricalNtfns(t *testing.T) {
	t.Parallel()

	const (
		numNtfns       = 5
		startingHeight = 10
	)

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

	// We'll start off by registered 5 clients for a confirmation
	// notification on the same transaction.
	confNtfns := make([]*chainntnfs.ConfNtfn, numNtfns)
	for i := uint64(0); i < numNtfns; i++ {
		confNtfns[i] = &chainntnfs.ConfNtfn{
			ConfID: i,
			TxID:   &zeroHash,
			Event:  chainntnfs.NewConfirmationEvent(1),
		}
		if _, err := n.RegisterConf(confNtfns[i]); err != nil {
			t.Fatalf("unable to register conf ntfn #%d: %v", i, err)
		}
	}

	// Ensure none of them have received the confirmation details.
	for i, ntfn := range confNtfns {
		select {
		case <-ntfn.Event.Confirmed:
			t.Fatalf("request #%d received unexpected confirmation "+
				"notification", i)
		default:
		}
	}

	// We'll assume a historical rescan was dispatched and found the
	// following confirmation details. We'll let the notifier know so that
	// it can stop watching at tip.
	expectedConfDetails := &chainntnfs.TxConfirmation{
		BlockHeight: startingHeight - 1,
	}
	err := n.UpdateConfDetails(*confNtfns[0].TxID, expectedConfDetails)
	if err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}

	// With the confirmation details retrieved, each client should now have
	// been notified of the confirmation.
	for i, ntfn := range confNtfns {
		select {
		case confDetails := <-ntfn.Event.Confirmed:
			assertConfDetails(t, confDetails, expectedConfDetails)
		default:
			t.Fatalf("request #%d expected to received "+
				"confirmation notification", i)
		}
	}

	// In order to ensure that the confirmation details are properly cached,
	// we'll register another client for the same transaction. We should not
	// see a historical rescan request and the confirmation notification
	// should come through immediately.
	extraConfNtfn := &chainntnfs.ConfNtfn{
		ConfID: numNtfns + 1,
		TxID:   &zeroHash,
		Event:  chainntnfs.NewConfirmationEvent(1),
	}
	historicalConfRescan, err := n.RegisterConf(extraConfNtfn)
	if err != nil {
		t.Fatalf("unable to register conf ntfn: %v", err)
	}
	if historicalConfRescan != nil {
		t.Fatal("received unexpected historical rescan request")
	}

	select {
	case confDetails := <-extraConfNtfn.Event.Confirmed:
		assertConfDetails(t, confDetails, expectedConfDetails)
	default:
		t.Fatal("expected to receive spend notification")
	}

	// Similarly, we'll do the same thing but for spend notifications.
	spendNtfns := make([]*chainntnfs.SpendNtfn, numNtfns)
	for i := uint64(0); i < numNtfns; i++ {
		spendNtfns[i] = &chainntnfs.SpendNtfn{
			SpendID:  i,
			OutPoint: zeroOutPoint,
			Event:    chainntnfs.NewSpendEvent(nil),
		}
		if _, err := n.RegisterSpend(spendNtfns[i]); err != nil {
			t.Fatalf("unable to register spend ntfn #%d: %v", i, err)
		}
	}

	// Ensure none of them have received the spend details.
	for i, ntfn := range spendNtfns {
		select {
		case <-ntfn.Event.Spend:
			t.Fatalf("request #%d received unexpected spend "+
				"notification", i)
		default:
		}
	}

	// We'll assume a historical rescan was dispatched and found the
	// following spend details. We'll let the notifier know so that it can
	// stop watching at tip.
	expectedSpendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &spendNtfns[0].OutPoint,
		SpenderTxHash:     &zeroHash,
		SpendingTx:        wire.NewMsgTx(2),
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight - 1,
	}
	err = n.UpdateSpendDetails(spendNtfns[0].OutPoint, expectedSpendDetails)
	if err != nil {
		t.Fatalf("unable to update spend details: %v", err)
	}

	// With the spend details retrieved, each client should now have been
	// notified of the spend.
	for i, ntfn := range spendNtfns {
		select {
		case spendDetails := <-ntfn.Event.Spend:
			assertSpendDetails(t, spendDetails, expectedSpendDetails)
		default:
			t.Fatalf("request #%d expected to received spend "+
				"notification", i)
		}
	}

	// Finally, in order to ensure that the spend details are properly
	// cached, we'll register another client for the same outpoint. We
	// should not see a historical rescan request and the spend notification
	// should come through immediately.
	extraSpendNtfn := &chainntnfs.SpendNtfn{
		SpendID:  numNtfns + 1,
		OutPoint: zeroOutPoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	historicalSpendRescan, err := n.RegisterSpend(extraSpendNtfn)
	if err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}
	if historicalSpendRescan != nil {
		t.Fatal("received unexpected historical rescan request")
	}

	select {
	case spendDetails := <-extraSpendNtfn.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails)
	default:
		t.Fatal("expected to receive spend notification")
	}
}

// TestTxNotifierCancelSpend ensures that a spend notification after a client
// has canceled their intent to receive one.
func TestTxNotifierCancelSpend(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

	// We'll register two notification requests. Only the second one will be
	// canceled.
	ntfn1 := &chainntnfs.SpendNtfn{
		SpendID:  0,
		OutPoint: zeroOutPoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	if _, err := n.RegisterSpend(ntfn1); err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}

	ntfn2 := &chainntnfs.SpendNtfn{
		SpendID:  1,
		OutPoint: zeroOutPoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	if _, err := n.RegisterSpend(ntfn2); err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}

	// Construct the spending details of the outpoint and create a dummy
	// block containing it.
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{PreviousOutPoint: ntfn1.OutPoint})
	spendTxHash := spendTx.TxHash()
	expectedSpendDetails := &chainntnfs.SpendDetail{
		SpentOutPoint:     &ntfn1.OutPoint,
		SpenderTxHash:     &spendTxHash,
		SpendingTx:        spendTx,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight + 1,
	}

	block := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx},
	})

	// Before extending the notifier's tip with the dummy block above, we'll
	// cancel the second request.
	n.CancelSpend(ntfn2.OutPoint, ntfn2.SpendID)

	err := n.ConnectTip(block.Hash(), startingHeight+1, block.Transactions())
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(startingHeight + 1); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// The first request should still be active, so we should receive a
	// spend notification with the correct spending details.
	select {
	case spendDetails := <-ntfn1.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails)
	default:
		t.Fatalf("expected to receive spend notification")
	}

	// The second one, however, should not have. The event's Spend channel
	// must have also been closed to indicate the caller that the TxNotifier
	// can no longer fulfill their canceled request.
	select {
	case _, ok := <-ntfn2.Event.Spend:
		if ok {
			t.Fatal("expected Spend channel to be closed")
		}
	default:
		t.Fatal("expected Spend channel to be closed")
	}
}

// TestTxNotifierConfReorg ensures that clients are notified of a reorg when a
// transaction for which they registered a confirmation notification has been
// reorged out of the chain.
func TestTxNotifierConfReorg(t *testing.T) {
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
	n := chainntnfs.NewTxNotifier(7, 100, hintCache, hintCache)

	// Tx 1 will be confirmed in block 9 and requires 2 confs.
	tx1Hash := tx1.TxHash()
	ntfn1 := chainntnfs.ConfNtfn{
		TxID:             &tx1Hash,
		NumConfirmations: tx1NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx1NumConfs),
	}
	if _, err := n.RegisterConf(&ntfn1); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	if err := n.UpdateConfDetails(*ntfn1.TxID, nil); err != nil {
		t.Fatalf("unable to deliver conf details: %v", err)
	}

	// Tx 2 will be confirmed in block 10 and requires 1 conf.
	tx2Hash := tx2.TxHash()
	ntfn2 := chainntnfs.ConfNtfn{
		TxID:             &tx2Hash,
		NumConfirmations: tx2NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx2NumConfs),
	}
	if _, err := n.RegisterConf(&ntfn2); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	if err := n.UpdateConfDetails(*ntfn2.TxID, nil); err != nil {
		t.Fatalf("unable to deliver conf details: %v", err)
	}

	// Tx 3 will be confirmed in block 10 and requires 2 confs.
	tx3Hash := tx3.TxHash()
	ntfn3 := chainntnfs.ConfNtfn{
		TxID:             &tx3Hash,
		NumConfirmations: tx3NumConfs,
		Event:            chainntnfs.NewConfirmationEvent(tx3NumConfs),
	}
	if _, err := n.RegisterConf(&ntfn3); err != nil {
		t.Fatalf("unable to register ntfn: %v", err)
	}

	if err := n.UpdateConfDetails(*ntfn3.TxID, nil); err != nil {
		t.Fatalf("unable to deliver conf details: %v", err)
	}

	// Sync chain to block 10. Txs 1 & 2 should be confirmed.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})
	if err := n.ConnectTip(nil, 8, block1.Transactions()); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(8); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}
	if err := n.ConnectTip(nil, 9, nil); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(9); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx2, &tx3},
	})
	if err := n.ConnectTip(nil, 10, block2.Transactions()); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(10); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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
	if err := n.DisconnectTip(10); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}

	if err := n.ConnectTip(nil, 10, nil); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(10); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	if err := n.ConnectTip(nil, 11, nil); err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(11); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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

	err := n.ConnectTip(block3.Hash(), 12, block3.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(12); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	err = n.ConnectTip(block4.Hash(), 13, block4.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(13); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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
		assertConfDetails(t, txConf, &expectedConf)
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
		assertConfDetails(t, txConf, &expectedConf)
	default:
		t.Fatalf("Expected confirmation for tx3")
	}
}

// TestTxNotifierSpendReorg ensures that clients are notified of a reorg when
// the spending transaction of an outpoint for which they registered a spend
// notification for has been reorged out of the chain.
func TestTxNotifierSpendReorg(t *testing.T) {
	t.Parallel()

	const startingHeight = 10
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

	// We'll have two outpoints that will be spent throughout the test. The
	// first will be spent and will not experience a reorg, while the second
	// one will.
	op1 := zeroOutPoint
	op1.Index = 1
	spendTx1 := wire.NewMsgTx(2)
	spendTx1.AddTxIn(&wire.TxIn{PreviousOutPoint: op1})
	spendTxHash1 := spendTx1.TxHash()
	expectedSpendDetails1 := &chainntnfs.SpendDetail{
		SpentOutPoint:     &op1,
		SpenderTxHash:     &spendTxHash1,
		SpendingTx:        spendTx1,
		SpenderInputIndex: 0,
		SpendingHeight:    startingHeight + 1,
	}

	op2 := zeroOutPoint
	op2.Index = 2
	spendTx2 := wire.NewMsgTx(2)
	spendTx2.AddTxIn(&wire.TxIn{PreviousOutPoint: zeroOutPoint})
	spendTx2.AddTxIn(&wire.TxIn{PreviousOutPoint: op2})
	spendTxHash2 := spendTx2.TxHash()

	// The second outpoint will experience a reorg and get re-spent at a
	// different height, so we'll need to construct the spend details for
	// before and after the reorg.
	expectedSpendDetails2BeforeReorg := chainntnfs.SpendDetail{
		SpentOutPoint:     &op2,
		SpenderTxHash:     &spendTxHash2,
		SpendingTx:        spendTx2,
		SpenderInputIndex: 1,
		SpendingHeight:    startingHeight + 2,
	}

	// The spend details after the reorg will be exactly the same, except
	// for the spend confirming at the next height.
	expectedSpendDetails2AfterReorg := expectedSpendDetails2BeforeReorg
	expectedSpendDetails2AfterReorg.SpendingHeight++

	// We'll register for a spend notification for each outpoint above.
	ntfn1 := &chainntnfs.SpendNtfn{
		SpendID:  78,
		OutPoint: op1,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	if _, err := n.RegisterSpend(ntfn1); err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}

	ntfn2 := &chainntnfs.SpendNtfn{
		SpendID:  21,
		OutPoint: op2,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	if _, err := n.RegisterSpend(ntfn2); err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}

	// We'll extend the chain by connecting a new block at tip. This block
	// will only contain the spending transaction of the first outpoint.
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx1},
	})
	err := n.ConnectTip(block1.Hash(), startingHeight+1, block1.Transactions())
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(startingHeight + 1); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should receive a spend notification for the first outpoint with
	// its correct spending details.
	select {
	case spendDetails := <-ntfn1.Event.Spend:
		assertSpendDetails(t, spendDetails, expectedSpendDetails1)
	default:
		t.Fatal("expected to receive spend details")
	}

	// We should not, however, receive one for the second outpoint as it has
	// yet to be spent.
	select {
	case <-ntfn2.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}

	// Now, we'll extend the chain again, this time with a block containing
	// the spending transaction of the second outpoint.
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx2},
	})
	err = n.ConnectTip(block2.Hash(), startingHeight+2, block2.Transactions())
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(startingHeight + 2); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should not receive another spend notification for the first
	// outpoint.
	select {
	case <-ntfn1.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}

	// We should receive one for the second outpoint.
	select {
	case spendDetails := <-ntfn2.Event.Spend:
		assertSpendDetails(
			t, spendDetails, &expectedSpendDetails2BeforeReorg,
		)
	default:
		t.Fatal("expected to receive spend details")
	}

	// Now, to replicate a chain reorg, we'll disconnect the block that
	// contained the spending transaction of the second outpoint.
	if err := n.DisconnectTip(startingHeight + 2); err != nil {
		t.Fatalf("unable to disconnect block: %v", err)
	}

	// No notifications should be dispatched for the first outpoint as it
	// was spent at a previous height.
	select {
	case <-ntfn1.Event.Spend:
		t.Fatal("received unexpected spend notification")
	case <-ntfn1.Event.Reorg:
		t.Fatal("received unexpected spend reorg notification")
	default:
	}

	// We should receive a reorg notification for the second outpoint.
	select {
	case <-ntfn2.Event.Spend:
		t.Fatal("received unexpected spend notification")
	case <-ntfn2.Event.Reorg:
	default:
		t.Fatal("expected spend reorg notification")
	}

	// We'll now extend the chain with an empty block, to ensure that we can
	// properly detect when an outpoint has been re-spent at a later height.
	emptyBlock := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(
		emptyBlock.Hash(), startingHeight+2, emptyBlock.Transactions(),
	)
	if err != nil {
		t.Fatalf("unable to disconnect block: %v", err)
	}
	if err := n.NotifyHeight(startingHeight + 2); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We shouldn't receive notifications for either of the outpoints.
	select {
	case <-ntfn1.Event.Spend:
		t.Fatal("received unexpected spend notification")
	case <-ntfn1.Event.Reorg:
		t.Fatal("received unexpected spend reorg notification")
	case <-ntfn2.Event.Spend:
		t.Fatal("received unexpected spend notification")
	case <-ntfn2.Event.Reorg:
		t.Fatal("received unexpected spend reorg notification")
	default:
	}

	// Finally, extend the chain with another block containing the same
	// spending transaction of the second outpoint.
	err = n.ConnectTip(
		block2.Hash(), startingHeight+3, block2.Transactions(),
	)
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(startingHeight + 3); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// We should now receive a spend notification once again for the second
	// outpoint containing the new spend details.
	select {
	case spendDetails := <-ntfn2.Event.Spend:
		assertSpendDetails(
			t, spendDetails, &expectedSpendDetails2AfterReorg,
		)
	default:
		t.Fatalf("expected to receive spend notification")
	}

	// Once again, we should not receive one for the first outpoint.
	select {
	case <-ntfn1.Event.Spend:
		t.Fatal("received unexpected spend notification")
	default:
	}
}

// TestTxNotifierConfirmHintCache ensures that the height hints for transactions
// are kept track of correctly with each new block connected/disconnected. This
// test also asserts that the height hints are not updated until the simulated
// historical dispatches have returned, and we know the transactions aren't
// already in the chain.
func TestTxNotifierConfirmHintCache(t *testing.T) {
	t.Parallel()

	const (
		startingHeight = 200
		txDummyHeight  = 201
		tx1Height      = 202
		tx2Height      = 203
	)

	// Initialize our TxNotifier instance backed by a height hint cache.
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

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

	if _, err := n.RegisterConf(ntfn1); err != nil {
		t.Fatalf("unable to register tx1: %v", err)
	}
	if _, err := n.RegisterConf(ntfn2); err != nil {
		t.Fatalf("unable to register tx2: %v", err)
	}

	// Both transactions should not have a height hint set, as RegisterConf
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

	err = n.ConnectTip(block1.Hash(), txDummyHeight, block1.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(txDummyHeight); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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
	if err := n.UpdateConfDetails(tx1Hash, nil); err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}
	if err := n.UpdateConfDetails(tx2Hash, nil); err != nil {
		t.Fatalf("unable to update conf details: %v", err)
	}

	// We'll create another block that will include the first transaction
	// and extend the chain.
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{&tx1},
	})

	err = n.ConnectTip(block2.Hash(), tx1Height, block2.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(tx1Height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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

	err = n.ConnectTip(block3.Hash(), tx2Height, block3.Transactions())
	if err != nil {
		t.Fatalf("Failed to connect block: %v", err)
	}
	if err := n.NotifyHeight(tx2Height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
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
	if err := n.DisconnectTip(tx2Height); err != nil {
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

// TestTxNotifierSpendHintCache ensures that the height hints for outpoints are
// kept track of correctly with each new block connected/disconnected. This test
// also asserts that the height hints are not updated until the simulated
// historical dispatches have returned, and we know the outpoints haven't
// already been spent in the chain.
func TestTxNotifierSpendHintCache(t *testing.T) {
	t.Parallel()

	const (
		startingHeight = 200
		dummyHeight    = 201
		op1Height      = 202
		op2Height      = 203
	)

	// Intiialize our TxNotifier instance backed by a height hint cache.
	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(startingHeight, 100, hintCache, hintCache)

	// Create two test outpoints and register them for spend notifications.
	op1 := wire.OutPoint{Hash: zeroHash, Index: 1}
	ntfn1 := &chainntnfs.SpendNtfn{
		OutPoint: op1,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	op2 := wire.OutPoint{Hash: zeroHash, Index: 2}
	ntfn2 := &chainntnfs.SpendNtfn{
		OutPoint: op2,
		Event:    chainntnfs.NewSpendEvent(nil),
	}

	if _, err := n.RegisterSpend(ntfn1); err != nil {
		t.Fatalf("unable to register spend for op1: %v", err)
	}
	if _, err := n.RegisterSpend(ntfn2); err != nil {
		t.Fatalf("unable to register spend for op2: %v", err)
	}

	// Both outpoints should not have a spend hint set upon registration, as
	// we must first determine whether they have already been spent in the
	// chain.
	_, err := hintCache.QuerySpendHint(op1)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}
	_, err = hintCache.QuerySpendHint(op2)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}

	// Create a new empty block and extend the chain.
	emptyBlock := btcutil.NewBlock(&wire.MsgBlock{})
	err = n.ConnectTip(
		emptyBlock.Hash(), dummyHeight, emptyBlock.Transactions(),
	)
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(dummyHeight); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Since we haven't called UpdateSpendDetails on any of the test
	// outpoints, this implies that there is a still a pending historical
	// rescan for them, so their spend hints should not be created/updated.
	_, err = hintCache.QuerySpendHint(op1)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}
	_, err = hintCache.QuerySpendHint(op2)
	if err != chainntnfs.ErrSpendHintNotFound {
		t.Fatalf("unexpected error when querying for height hint "+
			"expected: %v, got %v", chainntnfs.ErrSpendHintNotFound,
			err)
	}

	// Now, we'll simulate that their historical rescans have finished by
	// calling UpdateSpendDetails. This should allow their spend hints to be
	// updated upon every block connected/disconnected.
	if err := n.UpdateSpendDetails(ntfn1.OutPoint, nil); err != nil {
		t.Fatalf("unable to update spend details: %v", err)
	}
	if err := n.UpdateSpendDetails(ntfn2.OutPoint, nil); err != nil {
		t.Fatalf("unable to update spend details: %v", err)
	}

	// We'll create a new block that only contains the spending transaction
	// of the first outpoint.
	spendTx1 := wire.NewMsgTx(2)
	spendTx1.AddTxIn(&wire.TxIn{PreviousOutPoint: ntfn1.OutPoint})
	block1 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx1},
	})
	err = n.ConnectTip(block1.Hash(), op1Height, block1.Transactions())
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(op1Height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Both outpoints should have their spend hints reflect the height of
	// the new block being connected due to the first outpoint being spent
	// at this height, and the second outpoint still being unspent.
	op1Hint, err := hintCache.QuerySpendHint(ntfn1.OutPoint)
	if err != nil {
		t.Fatalf("unable to query for spend hint of op1: %v", err)
	}
	if op1Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op1Hint)
	}
	op2Hint, err := hintCache.QuerySpendHint(ntfn2.OutPoint)
	if err != nil {
		t.Fatalf("unable to query for spend hint of op2: %v", err)
	}
	if op2Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op2Hint)
	}

	// Then, we'll create another block that spends the second outpoint.
	spendTx2 := wire.NewMsgTx(2)
	spendTx2.AddTxIn(&wire.TxIn{PreviousOutPoint: ntfn2.OutPoint})
	block2 := btcutil.NewBlock(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{spendTx2},
	})
	err = n.ConnectTip(block2.Hash(), op2Height, block2.Transactions())
	if err != nil {
		t.Fatalf("unable to connect block: %v", err)
	}
	if err := n.NotifyHeight(op2Height); err != nil {
		t.Fatalf("unable to dispatch notifications: %v", err)
	}

	// Only the second outpoint should have its spend hint updated due to
	// being spent within the new block. The first outpoint's spend hint
	// should remain the same as it's already been spent before.
	op1Hint, err = hintCache.QuerySpendHint(ntfn1.OutPoint)
	if err != nil {
		t.Fatalf("unable to query for spend hint of op1: %v", err)
	}
	if op1Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op1Hint)
	}
	op2Hint, err = hintCache.QuerySpendHint(ntfn2.OutPoint)
	if err != nil {
		t.Fatalf("unable to query for spend hint of op2: %v", err)
	}
	if op2Hint != op2Height {
		t.Fatalf("expected hint %d, got %d", op2Height, op2Hint)
	}

	// Finally, we'll attempt do disconnect the last block in order to
	// simulate a chain reorg.
	if err := n.DisconnectTip(op2Height); err != nil {
		t.Fatalf("unable to disconnect block: %v", err)
	}

	// This should update the second outpoint's spend hint within the cache
	// to the previous height, as that's where its spending transaction was
	// included in within the chain. The first outpoint's spend hint should
	// remain the same.
	op1Hint, err = hintCache.QuerySpendHint(ntfn1.OutPoint)
	if err != nil {
		t.Fatalf("unable to query for spend hint of op1: %v", err)
	}
	if op1Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op1Hint)
	}
	op2Hint, err = hintCache.QuerySpendHint(ntfn2.OutPoint)
	if err != nil {
		t.Fatalf("unable to query for spend hint of op2: %v", err)
	}
	if op2Hint != op1Height {
		t.Fatalf("expected hint %d, got %d", op1Height, op2Hint)
	}
}

// TestTxNotifierTearDown ensures that the TxNotifier properly alerts clients
// that it is shutting down and will be unable to deliver notifications.
func TestTxNotifierTearDown(t *testing.T) {
	t.Parallel()

	hintCache := newMockHintCache()
	n := chainntnfs.NewTxNotifier(10, 100, hintCache, hintCache)

	// To begin the test, we'll register for a confirmation and spend
	// notification.
	confNtfn := &chainntnfs.ConfNtfn{
		TxID:             &zeroHash,
		NumConfirmations: 1,
		Event:            chainntnfs.NewConfirmationEvent(1),
	}
	if _, err := n.RegisterConf(confNtfn); err != nil {
		t.Fatalf("unable to register conf ntfn: %v", err)
	}

	spendNtfn := &chainntnfs.SpendNtfn{
		OutPoint: zeroOutPoint,
		Event:    chainntnfs.NewSpendEvent(nil),
	}
	if _, err := n.RegisterSpend(spendNtfn); err != nil {
		t.Fatalf("unable to register spend ntfn: %v", err)
	}

	// With the notifications registered, we'll now tear down the notifier.
	// The notification channels should be closed for notifications, whether
	// they have been dispatched or not, so we should not expect to receive
	// any more updates.
	n.TearDown()

	select {
	case _, ok := <-confNtfn.Event.Confirmed:
		if ok {
			t.Fatal("expected closed Confirmed channel for conf ntfn")
		}
	case _, ok := <-confNtfn.Event.Updates:
		if ok {
			t.Fatal("expected closed Updates channel for conf ntfn")
		}
	case _, ok := <-confNtfn.Event.NegativeConf:
		if ok {
			t.Fatal("expected closed NegativeConf channel for conf ntfn")
		}
	case _, ok := <-spendNtfn.Event.Spend:
		if ok {
			t.Fatal("expected closed Spend channel for spend ntfn")
		}
	case _, ok := <-spendNtfn.Event.Reorg:
		if ok {
			t.Fatalf("expected closed Reorg channel for spend ntfn")
		}
	default:
		t.Fatalf("expected closed notification channels for all ntfns")
	}

	// Now that the notifier is torn down, we should no longer be able to
	// register notification requests.
	if _, err := n.RegisterConf(confNtfn); err == nil {
		t.Fatal("expected confirmation registration to fail")
	}
	if _, err := n.RegisterSpend(spendNtfn); err == nil {
		t.Fatal("expected spend registration to fail")
	}
}

func assertConfDetails(t *testing.T, result, expected *chainntnfs.TxConfirmation) {
	t.Helper()

	if result.BlockHeight != expected.BlockHeight {
		t.Fatalf("Incorrect block height in confirmation details: "+
			"expected %d, got %d", expected.BlockHeight,
			result.BlockHeight)
	}
	if !result.BlockHash.IsEqual(expected.BlockHash) {
		t.Fatalf("Incorrect block hash in confirmation details: "+
			"expected %d, got %d", expected.BlockHash,
			result.BlockHash)
	}
	if result.TxIndex != expected.TxIndex {
		t.Fatalf("Incorrect tx index in confirmation details: "+
			"expected %d, got %d", expected.TxIndex, result.TxIndex)
	}
}

func assertSpendDetails(t *testing.T, result, expected *chainntnfs.SpendDetail) {
	t.Helper()

	if *result.SpentOutPoint != *expected.SpentOutPoint {
		t.Fatalf("expected spent outpoint %v, got %v",
			expected.SpentOutPoint, result.SpentOutPoint)
	}
	if !result.SpenderTxHash.IsEqual(expected.SpenderTxHash) {
		t.Fatalf("expected spender tx hash %v, got %v",
			expected.SpenderTxHash, result.SpenderTxHash)
	}
	if result.SpenderInputIndex != expected.SpenderInputIndex {
		t.Fatalf("expected spender input index %d, got %d",
			expected.SpenderInputIndex, result.SpenderInputIndex)
	}
	if result.SpendingHeight != expected.SpendingHeight {
		t.Fatalf("expected spending height %d, got %d",
			expected.SpendingHeight, result.SpendingHeight)
	}
}
