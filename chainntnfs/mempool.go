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

	// event is the channel that will be sent upon once the target outpoint
	// is spent.
	event chan *SpendDetail

	// cancel cancels the subscription.
	cancel chan struct{}
}

// newMempoolSpendEvent returns a new instance of MempoolSpendEvent.
func newMempoolSpendEvent() *MempoolSpendEvent {
	sub := &MempoolSpendEvent{
		event:  make(chan *SpendDetail, 1),
		cancel: make(chan struct{}),
	}

	// Mount the receive only channel to the event channel.
	sub.Spend = (<-chan *SpendDetail)(sub.event)

	return sub
}

// Cancel cancels the subscription.
func (m *MempoolSpendEvent) Cancel() {
	close(m.cancel)
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

	Log.Debugf("Subscribing mempool event for input %s", outpoint)

	// Get the current subscribers for this input or create a new one.
	clients := &lnutils.SyncMap[uint64, *MempoolSpendEvent]{}
	clients, _ = m.subscribedInputs.LoadOrStore(outpoint, clients)

	// Create a new subscription.
	sub := newMempoolSpendEvent()

	// Add the subscriber with a unique id.
	subscriptionID := m.sCounter.Add(1)
	clients.Store(subscriptionID, sub)

	// Update the subscribed inputs.
	m.subscribedInputs.Store(outpoint, clients)

	return sub
}

// Unsubscribe removes the subscription for the given outpoint.
func (m *MempoolNotifier) Unsubscribe(outpoint wire.OutPoint) {
	Log.Debugf("Unsubscribing MempoolSpendEvent for input %s", outpoint)
	m.subscribedInputs.Delete(outpoint)
}

// ProcessRelevantSpendTx takes a transaction and checks whether it spends any
// of the subscribed inputs. If so, spend notifications are sent to the
// relevant subscribers.
func (m *MempoolNotifier) ProcessRelevantSpendTx(tx *btcutil.Tx) {
	Log.Tracef("Processing mempool tx %s", tx.Hash())
	defer Log.Tracef("Finished processing mempool tx %s", tx.Hash())

	// Get the spent inputs of interest.
	spentInputs := m.findRelevantInputs(tx)

	// Notify the subscribers.
	m.notifySpent(spentInputs)
}

// TearDown stops the notifier and cleans up resources.
func (m *MempoolNotifier) TearDown() {
	Log.Infof("Stopping mempool notifier")
	close(m.quit)
	m.wg.Wait()
}

// findRelevantInputs takes a transaction to find the subscribed inputs and
// returns them.
func (m *MempoolNotifier) findRelevantInputs(tx *btcutil.Tx) inputsWithTx {
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
		Log.Infof("Found input %s, spent in %s", op, txid)

		// Construct the spend details.
		details := &SpendDetail{
			SpentOutPoint:     op,
			SpenderTxHash:     txid,
			SpendingTx:        tx.MsgTx().Copy(),
			SpenderInputIndex: uint32(i),
			SpendingHeight:    0,
		}
		watchedInputs[*op] = details
	}

	return watchedInputs
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

		Log.Debugf("Notifying client %d", id)

		// Send the spend details to the subscriber.
		select {
		case sub.event <- detail:
			Log.Debugf("Notified mempool spent for input %s", op)

		case <-sub.cancel:
			Log.Debugf("Subscription canceled, skipped notifying "+
				"mempool spent for input %s", op)

			// Find all the subscribers for this outpoint.
			clients, loaded := m.subscribedInputs.Load(op)
			if !loaded {
				Log.Errorf("Client %d not found", id)
				return
			}

			// Delete the specific subscriber.
			clients.Delete(id)

			// Update the subscribers map.
			m.subscribedInputs.Store(op, clients)

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
		Log.Debugf("Notifying the spend of %s in tx %s", op, txid)

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
