package chainntnfs

import (
	"sync"

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
	subscribedInputs lnutils.SyncMap[wire.OutPoint, *MempoolSpendEvent]

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
		subscribedInputs: lnutils.SyncMap[
			wire.OutPoint, *MempoolSpendEvent,
		]{},
		quit: make(chan struct{}),
	}
}

// SubscribeInput takes an outpoint of the input and returns a channel that the
// subscriber can listen to for the spending event.
func (m *MempoolNotifier) SubscribeInput(
	outpoint wire.OutPoint) *MempoolSpendEvent {

	Log.Debugf("Subscribing mempool event for input %s", outpoint)
	sub := newMempoolSpendEvent()

	m.subscribedInputs.Store(outpoint, sub)

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
	// notify is a helper closure that constructs a spend detail and sends
	// it to the subscribers.
	//
	// NOTE: must be used inside a goroutine.
	notify := func(detail *SpendDetail, op wire.OutPoint) {
		defer m.wg.Done()

		txid := detail.SpendingTx.TxHash()
		Log.Debugf("Notifying the spend of %s in tx %s", op, txid)

		// Load the subscriber.
		sub, loaded := m.subscribedInputs.Load(op)
		if !loaded {
			// TODO(yy): this is possible if the subscriber cancels
			// the subscription?
			Log.Errorf("Sub not found for %s", op)
			return
		}

		// Send the spend details to the subscriber.
		select {
		case sub.event <- detail:
			Log.Debugf("Notified mempool spent for input %s", op)

		case <-sub.cancel:
			Log.Debugf("Subscription canceled, skipped notifying "+
				"mempool spent for input %s", op)

			// Delete the subscriber.
			m.subscribedInputs.Delete(op)

		case <-m.quit:
			Log.Debugf("Mempool notifier quit, skipped notifying "+
				"mempool spent for input %s", op)
		}
	}

	// Iterate the spent inputs to notify the subscribers concurrently.
	for op, tx := range spentInputs {
		m.wg.Add(1)
		go notify(tx, op)
	}
}
