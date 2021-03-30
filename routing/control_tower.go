package routing

import (
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/multimutex"
	"github.com/lightningnetwork/lnd/queue"
)

// ControlTower tracks all outgoing payments made, whose primary purpose is to
// prevent duplicate payments to the same payment hash. In production, a
// persistent implementation is preferred so that tracking can survive across
// restarts. Payments are transitioned through various payment states, and the
// ControlTower interface provides access to driving the state transitions.
type ControlTower interface {
	// This method checks that no suceeded payment exist for this payment
	// hash.
	InitPayment(lntypes.Hash, *channeldb.PaymentCreationInfo) error

	// RegisterAttempt atomically records the provided HTLCAttemptInfo.
	RegisterAttempt(lntypes.Hash, *channeldb.HTLCAttemptInfo) error

	// SettleAttempt marks the given attempt settled with the preimage. If
	// this is a multi shard payment, this might implicitly mean the the
	// full payment succeeded.
	//
	// After invoking this method, InitPayment should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. The provided preimage is atomically saved to the DB
	// for record keeping.
	SettleAttempt(lntypes.Hash, uint64, *channeldb.HTLCSettleInfo) (
		*channeldb.HTLCAttempt, error)

	// FailAttempt marks the given payment attempt failed.
	FailAttempt(lntypes.Hash, uint64, *channeldb.HTLCFailInfo) (
		*channeldb.HTLCAttempt, error)

	// FetchPayment fetches the payment corresponding to the given payment
	// hash.
	FetchPayment(paymentHash lntypes.Hash) (*channeldb.MPPayment, error)

	// Fail transitions a payment into the Failed state, and records the
	// ultimate reason the payment failed. Note that this should only be
	// called when all active active attempts are already failed. After
	// invoking this method, InitPayment should return nil on its next call
	// for this payment hash, allowing the user to make a subsequent
	// payment.
	Fail(lntypes.Hash, channeldb.FailureReason) error

	// FetchInFlightPayments returns all payments with status InFlight.
	FetchInFlightPayments() ([]*channeldb.MPPayment, error)

	// SubscribePayment subscribes to updates for the payment with the given
	// hash. A first update with the current state of the payment is always
	// sent out immediately.
	SubscribePayment(paymentHash lntypes.Hash) (*ControlTowerSubscriber,
		error)
}

// ControlTowerSubscriber contains the state for a payment update subscriber.
type ControlTowerSubscriber struct {
	// Updates is the channel over which *channeldb.MPPayment updates can be
	// received.
	Updates <-chan interface{}

	queue *queue.ConcurrentQueue
	quit  chan struct{}
}

// newControlTowerSubscriber instantiates a new subscriber state object.
func newControlTowerSubscriber() *ControlTowerSubscriber {
	// Create a queue for payment updates.
	queue := queue.NewConcurrentQueue(20)
	queue.Start()

	return &ControlTowerSubscriber{
		Updates: queue.ChanOut(),
		queue:   queue,
		quit:    make(chan struct{}),
	}
}

// Close signals that the subscriber is no longer interested in updates.
func (s *ControlTowerSubscriber) Close() {
	// Close quit channel so that any pending writes to the queue are
	// cancelled.
	close(s.quit)

	// Stop the queue goroutine so that it won't leak.
	s.queue.Stop()
}

// controlTower is persistent implementation of ControlTower to restrict
// double payment sending.
type controlTower struct {
	db *channeldb.PaymentControl

	subscribers    map[lntypes.Hash][]*ControlTowerSubscriber
	subscribersMtx sync.Mutex

	// paymentsMtx provides synchronization on the payment level to ensure
	// that no race conditions occur in between updating the database and
	// sending a notification.
	paymentsMtx *multimutex.HashMutex
}

// NewControlTower creates a new instance of the controlTower.
func NewControlTower(db *channeldb.PaymentControl) ControlTower {
	return &controlTower{
		db:          db,
		subscribers: make(map[lntypes.Hash][]*ControlTowerSubscriber),
		paymentsMtx: multimutex.NewHashMutex(),
	}
}

// InitPayment checks or records the given PaymentCreationInfo with the DB,
// making sure it does not already exist as an in-flight payment. Then this
// method returns successfully, the payment is guranteeed to be in the InFlight
// state.
func (p *controlTower) InitPayment(paymentHash lntypes.Hash,
	info *channeldb.PaymentCreationInfo) error {

	return p.db.InitPayment(paymentHash, info)
}

// RegisterAttempt atomically records the provided HTLCAttemptInfo to the
// DB.
func (p *controlTower) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *channeldb.HTLCAttemptInfo) error {

	p.paymentsMtx.Lock(paymentHash)
	defer p.paymentsMtx.Unlock(paymentHash)

	payment, err := p.db.RegisterAttempt(paymentHash, attempt)
	if err != nil {
		return err
	}

	// Notify subscribers of the attempt registration.
	p.notifySubscribers(paymentHash, payment)

	return nil
}

// SettleAttempt marks the given attempt settled with the preimage. If
// this is a multi shard payment, this might implicitly mean the the
// full payment succeeded.
func (p *controlTower) SettleAttempt(paymentHash lntypes.Hash,
	attemptID uint64, settleInfo *channeldb.HTLCSettleInfo) (
	*channeldb.HTLCAttempt, error) {

	p.paymentsMtx.Lock(paymentHash)
	defer p.paymentsMtx.Unlock(paymentHash)

	payment, err := p.db.SettleAttempt(paymentHash, attemptID, settleInfo)
	if err != nil {
		return nil, err
	}

	// Notify subscribers of success event.
	p.notifySubscribers(paymentHash, payment)

	return payment.GetAttempt(attemptID)
}

// FailAttempt marks the given payment attempt failed.
func (p *controlTower) FailAttempt(paymentHash lntypes.Hash,
	attemptID uint64, failInfo *channeldb.HTLCFailInfo) (
	*channeldb.HTLCAttempt, error) {

	p.paymentsMtx.Lock(paymentHash)
	defer p.paymentsMtx.Unlock(paymentHash)

	payment, err := p.db.FailAttempt(paymentHash, attemptID, failInfo)
	if err != nil {
		return nil, err
	}

	// Notify subscribers of failed attempt.
	p.notifySubscribers(paymentHash, payment)

	return payment.GetAttempt(attemptID)
}

// FetchPayment fetches the payment corresponding to the given payment hash.
func (p *controlTower) FetchPayment(paymentHash lntypes.Hash) (
	*channeldb.MPPayment, error) {

	return p.db.FetchPayment(paymentHash)
}

// Fail transitions a payment into the Failed state, and records the reason the
// payment failed. After invoking this method, InitPayment should return nil on
// its next call for this payment hash, allowing the switch to make a
// subsequent payment.
func (p *controlTower) Fail(paymentHash lntypes.Hash,
	reason channeldb.FailureReason) error {

	p.paymentsMtx.Lock(paymentHash)
	defer p.paymentsMtx.Unlock(paymentHash)

	payment, err := p.db.Fail(paymentHash, reason)
	if err != nil {
		return err
	}

	// Notify subscribers of fail event.
	p.notifySubscribers(paymentHash, payment)

	return nil
}

// FetchInFlightPayments returns all payments with status InFlight.
func (p *controlTower) FetchInFlightPayments() ([]*channeldb.MPPayment, error) {
	return p.db.FetchInFlightPayments()
}

// SubscribePayment subscribes to updates for the payment with the given hash. A
// first update with the current state of the payment is always sent out
// immediately.
func (p *controlTower) SubscribePayment(paymentHash lntypes.Hash) (
	*ControlTowerSubscriber, error) {

	// Take lock before querying the db to prevent missing or duplicating an
	// update.
	p.paymentsMtx.Lock(paymentHash)
	defer p.paymentsMtx.Unlock(paymentHash)

	payment, err := p.db.FetchPayment(paymentHash)
	if err != nil {
		return nil, err
	}

	subscriber := newControlTowerSubscriber()

	// Always write current payment state to the channel.
	subscriber.queue.ChanIn() <- payment

	// Payment is currently in flight. Register this subscriber for further
	// updates. Otherwise this update is the final update and the incoming
	// channel can be closed. This will close the queue's outgoing channel
	// when all updates have been written.
	if payment.Status == channeldb.StatusInFlight {
		p.subscribersMtx.Lock()
		p.subscribers[paymentHash] = append(
			p.subscribers[paymentHash], subscriber,
		)
		p.subscribersMtx.Unlock()
	} else {
		close(subscriber.queue.ChanIn())
	}

	return subscriber, nil
}

// notifySubscribers sends a final payment event to all subscribers of this
// payment. The channel will be closed after this. Note that this function must
// be executed atomically (by means of a lock) with the database update to
// guarantuee consistency of the notifications.
func (p *controlTower) notifySubscribers(paymentHash lntypes.Hash,
	event *channeldb.MPPayment) {

	// Get all subscribers for this payment.
	p.subscribersMtx.Lock()
	list, ok := p.subscribers[paymentHash]
	if !ok {
		p.subscribersMtx.Unlock()
		return
	}

	// If the payment reached a terminal state, the subscriber list can be
	// cleared. There won't be any more updates.
	terminal := event.Status != channeldb.StatusInFlight
	if terminal {
		delete(p.subscribers, paymentHash)
	}
	p.subscribersMtx.Unlock()

	// Notify all subscribers of the event.
	for _, subscriber := range list {
		select {
		case subscriber.queue.ChanIn() <- event:
			// If this event is the last, close the incoming channel
			// of the queue. This will signal the subscriber that
			// there won't be any more updates.
			if terminal {
				close(subscriber.queue.ChanIn())
			}

		// If subscriber disappeared, skip notification. For further
		// notifications, we'll keep skipping over this subscriber.
		case <-subscriber.quit:
		}
	}
}
