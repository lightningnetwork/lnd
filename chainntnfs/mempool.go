package chainntnfs

import (
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnutils"
)

// inputsWithTx is a map of outpoints to the tx that spends them.
type inputsWithTx map[wire.OutPoint]*SpendDetail

// MempoolNotifier defines an internal mempool notifier that's used to notify
// the spending of given inputs. This is mounted to either `BitcoindNotifier`
// or `BtcdNotifier` depending on the chain backend.
type MempoolNotifier struct {
	wg sync.WaitGroup

	// subscribedInputs stores the inputs that we want to watch their
	// spending event for.
	subscribedInputs *lnutils.SyncMap[wire.OutPoint,
		*lnutils.SyncMap[uint64, *MempoolSpendEvent]]

	// sCounter is used to generate unique subscription IDs.
	sCounter atomic.Uint64

	// quit is closed when the notifier is torn down.
	quit chan struct{}
}

// MempoolSpendEvent is returned to the subscriber to watch for the spending
// event for the requested input.
type MempoolSpendEvent struct {
	// Spend is a receive only channel which will be sent upon once the
	// target outpoint has been spent.
	//
	// NOTE: This channel must be buffered.
	Spend <-chan *SpendDetail

	// id is the unique identifier of this subscription.
	id uint64

	// outpoint is the subscribed outpoint.
	outpoint wire.OutPoint

	// event is the channel that will be sent upon once the target outpoint
	// is spent.
	event chan *SpendDetail

	// cancel cancels the subscription.
	cancel chan struct{}
}

// newMempoolSpendEvent returns a new instance of MempoolSpendEvent.
func newMempoolSpendEvent(id uint64, op wire.OutPoint) *MempoolSpendEvent {
	sub := &MempoolSpendEvent{
		id:       id,
		outpoint: op,
		event:    make(chan *SpendDetail, 1),
		cancel:   make(chan struct{}),
	}

	// Mount the receive only channel to the event channel.
	sub.Spend = (<-chan *SpendDetail)(sub.event)

	return sub
}

// NewMempoolNotifier takes a chain connection and returns a new mempool
// notifier.
func NewMempoolNotifier() *MempoolNotifier {
	return &MempoolNotifier{
		subscribedInputs: &lnutils.SyncMap[
			wire.OutPoint, *lnutils.SyncMap[
				uint64, *MempoolSpendEvent,
			]]{},
		quit: make(chan struct{}),
	}
}

// SubscribeInput takes an outpoint of the input and returns a channel that the
// subscriber can listen to for the spending event.
func (m *MempoolNotifier) SubscribeInput(
	outpoint wire.OutPoint) *MempoolSpendEvent {

	// Get the current subscribers for this input or create a new one.
	clients := &lnutils.SyncMap[uint64, *MempoolSpendEvent]{}
	clients, _ = m.subscribedInputs.LoadOrStore(outpoint, clients)

	// Increment the subscription counter and return the new value.
	subscriptionID := m.sCounter.Add(1)

	// Create a new subscription.
	sub := newMempoolSpendEvent(subscriptionID, outpoint)

	// Add the subscriber with a unique id.
	clients.Store(subscriptionID, sub)

	// Update the subscribed inputs.
	m.subscribedInputs.Store(outpoint, clients)

	Log.Debugf("Subscribed(id=%v) mempool event for input=%s",
		subscriptionID, outpoint)

	return sub
}

// UnsubscribeInput removes all the subscriptions for the given outpoint.
func (m *MempoolNotifier) UnsubscribeInput(outpoint wire.OutPoint) {
	Log.Debugf("Unsubscribing MempoolSpendEvent for input %s", outpoint)
	m.subscribedInputs.Delete(outpoint)
}

// UnsubscribeEvent removes a given subscriber for the given MempoolSpendEvent.
func (m *MempoolNotifier) UnsubscribeEvent(sub *MempoolSpendEvent) {
	Log.Debugf("Unsubscribing(id=%v) MempoolSpendEvent for input=%s",
		sub.id, sub.outpoint)

	// Load all the subscribers for this input.
	clients, loaded := m.subscribedInputs.Load(sub.outpoint)
	if !loaded {
		Log.Debugf("No subscribers for input %s", sub.outpoint)
		return
	}

	// Load the subscriber.
	subscriber, loaded := clients.Load(sub.id)
	if !loaded {
		Log.Debugf("No subscribers for input %s with id %v",
			sub.outpoint, sub.id)
		return
	}

	// Close the cancel channel in case it's been used in a goroutine.
	close(subscriber.cancel)

	// Remove the subscriber.
	clients.Delete(sub.id)
}

// UnsubsribeConfirmedSpentTx takes a transaction and removes the subscriptions
// identified using its inputs.
func (m *MempoolNotifier) UnsubsribeConfirmedSpentTx(tx *btcutil.Tx) {
	Log.Tracef("Unsubscribe confirmed tx %s", tx.Hash())

	// Get the spent inputs of interest.
	spentInputs, err := m.findRelevantInputs(tx)
	if err != nil {
		Log.Errorf("Unable to find relevant inputs for tx %s: %v",
			tx.Hash(), err)

		return
	}

	// Unsubscribe the subscribers.
	for outpoint := range spentInputs {
		m.UnsubscribeInput(outpoint)
	}

	Log.Tracef("Finished unsubscribing confirmed tx %s, found %d inputs",
		tx.Hash(), len(spentInputs))
}

// ProcessRelevantSpendTx takes a transaction and checks whether it spends any
// of the subscribed inputs. If so, spend notifications are sent to the
// relevant subscribers.
func (m *MempoolNotifier) ProcessRelevantSpendTx(tx *btcutil.Tx) error {
	Log.Tracef("Processing mempool tx %s", tx.Hash())
	defer Log.Tracef("Finished processing mempool tx %s", tx.Hash())

	// Get the spent inputs of interest.
	spentInputs, err := m.findRelevantInputs(tx)
	if err != nil {
		return err
	}

	// Notify the subscribers.
	m.notifySpent(spentInputs)

	return nil
}

// TearDown stops the notifier and cleans up resources.
func (m *MempoolNotifier) TearDown() {
	Log.Infof("Stopping mempool notifier")
	defer Log.Debug("mempool notifier stopped")

	close(m.quit)
	m.wg.Wait()
}

// findRelevantInputs takes a transaction to find the subscribed inputs and
// returns them.
func (m *MempoolNotifier) findRelevantInputs(tx *btcutil.Tx) (inputsWithTx,
	error) {

	txid := tx.Hash()
	watchedInputs := make(inputsWithTx)

	// NOTE: we may have found multiple targeted inputs in the same tx.
	for i, input := range tx.MsgTx().TxIn {
		op := &input.PreviousOutPoint

		// Check whether this input is subscribed.
		_, loaded := m.subscribedInputs.Load(*op)
		if !loaded {
			continue
		}

		// If found, save it to watchedInputs to notify the
		// subscriber later.
		Log.Debugf("Found input %s, spent in %s", op, txid)

		// Construct the spend details.
		details := &SpendDetail{
			SpentOutPoint:     op,
			SpenderTxHash:     txid,
			SpendingTx:        tx.MsgTx().Copy(),
			SpenderInputIndex: uint32(i),
			SpendingHeight:    0,
		}
		watchedInputs[*op] = details

		// Sanity check the witness stack. If it's not empty, continue
		// to next iteration.
		if details.HasSpenderWitness() {
			continue
		}

		// Return an error if the witness data is not present in the
		// spending transaction.
		Log.Criticalf("Found spending tx for outpoint=%v in mempool, "+
			"but the transaction %v does not have witness",
			op, details.SpendingTx.TxHash())

		return nil, ErrEmptyWitnessStack
	}

	return watchedInputs, nil
}

// notifySpent iterates all the spentInputs and notifies the subscribers about
// the spent details.
func (m *MempoolNotifier) notifySpent(spentInputs inputsWithTx) {
	// notifySingle sends a notification to a single subscriber about the
	// spending event.
	//
	// NOTE: must be used inside a goroutine.
	notifySingle := func(id uint64, sub *MempoolSpendEvent,
		op wire.OutPoint, detail *SpendDetail) {

		defer m.wg.Done()

		// Send the spend details to the subscriber.
		select {
		case sub.event <- detail:
			Log.Debugf("Notified(id=%v) mempool spent for input %s",
				sub.id, op)

		case <-sub.cancel:
			Log.Debugf("Subscription(id=%v) canceled, skipped "+
				"notifying spent for input %s", sub.id, op)

		case <-m.quit:
			Log.Debugf("Mempool notifier quit, skipped notifying "+
				"mempool spent for input %s", op)
		}
	}

	// notifyAll is a helper closure that constructs a spend detail and
	// sends it to all the subscribers of that particular input.
	//
	// NOTE: must be used inside a goroutine.
	notifyAll := func(detail *SpendDetail, op wire.OutPoint) {
		defer m.wg.Done()

		txid := detail.SpendingTx.TxHash()
		Log.Debugf("Notifying all clients for the spend of %s in tx %s",
			op, txid)

		// Load the subscriber.
		subs, loaded := m.subscribedInputs.Load(op)
		if !loaded {
			Log.Errorf("Sub not found for %s", op)
			return
		}

		// Iterate all the subscribers for this input and notify them.
		subs.ForEach(func(id uint64, sub *MempoolSpendEvent) error {
			m.wg.Add(1)
			go notifySingle(id, sub, op, detail)

			return nil
		})
	}

	// Iterate the spent inputs to notify the subscribers concurrently.
	for op, tx := range spentInputs {
		op, tx := op, tx

		m.wg.Add(1)
		go notifyAll(tx, op)
	}
}
