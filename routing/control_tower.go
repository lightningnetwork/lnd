package routing

import (
	"sync"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/multimutex"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/queue"
)

// ControlTower tracks all outgoing payments made, whose primary purpose is to
// prevent duplicate payments to the same payment hash. In production, a
// persistent implementation is preferred so that tracking can survive across
// restarts. Payments are transitioned through various payment states, and the
// ControlTower interface provides access to driving the state transitions.
type ControlTower interface {
	// InitPayment initializes a new payment with the given payment hash and
	// also notifies subscribers of the payment creation.
	//
	// NOTE: Subscribers should be notified by the new state of the payment.
	InitPayment(lntypes.Hash, *paymentsdb.PaymentCreationInfo) error

	// DeleteFailedAttempts removes all failed HTLCs from the db. It should
	// be called for a given payment whenever all inflight htlcs are
	// completed, and the payment has reached a final settled state.
	DeleteFailedAttempts(lntypes.Hash) error

	// RegisterAttempt atomically records the provided HTLCAttemptInfo.
	//
	// NOTE: Subscribers should be notified by the new state of the payment.
	RegisterAttempt(lntypes.Hash, *paymentsdb.HTLCAttemptInfo) error

	// SettleAttempt marks the given attempt settled with the preimage. If
	// this is a multi shard payment, this might implicitly mean the the
	// full payment succeeded.
	//
	// After invoking this method, InitPayment should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. The provided preimage is atomically saved to the DB
	// for record keeping.
	//
	// NOTE: Subscribers should be notified by the new state of the payment.
	SettleAttempt(lntypes.Hash, uint64, *paymentsdb.HTLCSettleInfo) (
		*paymentsdb.HTLCAttempt, error)

	// FailAttempt marks the given payment attempt failed.
	//
	// NOTE: Subscribers should be notified by the new state of the payment.
	FailAttempt(lntypes.Hash, uint64, *paymentsdb.HTLCFailInfo) (
		*paymentsdb.HTLCAttempt, error)

	// FetchPayment fetches the payment corresponding to the given payment
	// hash.
	FetchPayment(paymentHash lntypes.Hash) (paymentsdb.DBMPPayment, error)

	// FailPayment transitions a payment into the Failed state, and records
	// the ultimate reason the payment failed. Note that this should only
	// be called when all active attempts are already failed. After
	// invoking this method, InitPayment should return nil on its next call
	// for this payment hash, allowing the user to make a subsequent
	// payment.
	//
	// NOTE: Subscribers should be notified by the new state of the payment.
	FailPayment(lntypes.Hash, paymentsdb.FailureReason) error

	// FetchInFlightPayments returns all payments with status InFlight.
	FetchInFlightPayments() ([]*paymentsdb.MPPayment, error)

	// SubscribePayment subscribes to updates for the payment with the given
	// hash. A first update with the current state of the payment is always
	// sent out immediately.
	SubscribePayment(paymentHash lntypes.Hash) (ControlTowerSubscriber,
		error)

	// SubscribeAllPayments subscribes to updates for all payments. A first
	// update with the current state of every inflight payment is always
	// sent out immediately.
	SubscribeAllPayments() (ControlTowerSubscriber, error)
}

// ControlTowerSubscriber contains the state for a payment update subscriber.
type ControlTowerSubscriber interface {
	// Updates is the channel over which *channeldb.MPPayment updates can be
	// received.
	Updates() <-chan interface{}

	// Close signals that the subscriber is no longer interested in updates.
	Close()
}

// ControlTowerSubscriberImpl contains the state for a payment update
// subscriber.
type controlTowerSubscriberImpl struct {
	updates <-chan interface{}
	queue   *queue.ConcurrentQueue
	quit    chan struct{}
}

// newControlTowerSubscriber instantiates a new subscriber state object.
func newControlTowerSubscriber() *controlTowerSubscriberImpl {
	// Create a queue for payment updates.
	queue := queue.NewConcurrentQueue(20)
	queue.Start()

	return &controlTowerSubscriberImpl{
		updates: queue.ChanOut(),
		queue:   queue,
		quit:    make(chan struct{}),
	}
}

// Close signals that the subscriber is no longer interested in updates.
func (s *controlTowerSubscriberImpl) Close() {
	// Close quit channel so that any pending writes to the queue are
	// cancelled.
	close(s.quit)

	// Stop the queue goroutine so that it won't leak.
	s.queue.Stop()
}

// Updates is the channel over which *channeldb.MPPayment updates can be
// received.
func (s *controlTowerSubscriberImpl) Updates() <-chan interface{} {
	return s.updates
}

// controlTower is persistent implementation of ControlTower to restrict
// double payment sending.
type controlTower struct {
	db paymentsdb.DB

	// subscriberIndex is used to provide a unique id for each subscriber
	// to all payments. This is used to easily remove the subscriber when
	// necessary.
	subscriberIndex        uint64
	subscribersAllPayments map[uint64]*controlTowerSubscriberImpl
	subscribers            map[lntypes.Hash][]*controlTowerSubscriberImpl
	subscribersMtx         sync.Mutex

	// paymentsMtx provides synchronization on the payment level to ensure
	// that no race conditions occur in between updating the database and
	// sending a notification.
	paymentsMtx *multimutex.Mutex[lntypes.Hash]
}

// NewControlTower creates a new instance of the controlTower.
func NewControlTower(db paymentsdb.DB) ControlTower {
	return &controlTower{
		db: db,
		subscribersAllPayments: make(
			map[uint64]*controlTowerSubscriberImpl,
		),
		subscribers: make(map[lntypes.Hash][]*controlTowerSubscriberImpl),
		paymentsMtx: multimutex.NewMutex[lntypes.Hash](),
	}
}

// InitPayment checks or records the given PaymentCreationInfo with the DB,
// making sure it does not already exist as an in-flight payment. Then this
// method returns successfully, the payment is guaranteed to be in the
// Initiated state.
func (p *controlTower) InitPayment(paymentHash lntypes.Hash,
	info *paymentsdb.PaymentCreationInfo) error {

	err := p.db.InitPayment(paymentHash, info)
	if err != nil {
		return err
	}

	// Take lock before querying the db to prevent missing or duplicating
	// an update.
	p.paymentsMtx.Lock(paymentHash)
	defer p.paymentsMtx.Unlock(paymentHash)

	payment, err := p.db.FetchPayment(paymentHash)
	if err != nil {
		return err
	}

	p.notifySubscribers(paymentHash, payment)

	return nil
}

// DeleteFailedAttempts deletes all failed htlcs if the payment was
// successfully settled.
func (p *controlTower) DeleteFailedAttempts(paymentHash lntypes.Hash) error {
	return p.db.DeleteFailedAttempts(paymentHash)
}

// RegisterAttempt atomically records the provided HTLCAttemptInfo to the
// DB.
func (p *controlTower) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *paymentsdb.HTLCAttemptInfo) error {

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
	attemptID uint64, settleInfo *paymentsdb.HTLCSettleInfo) (
	*paymentsdb.HTLCAttempt, error) {

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
	attemptID uint64, failInfo *paymentsdb.HTLCFailInfo) (
	*paymentsdb.HTLCAttempt, error) {

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
	paymentsdb.DBMPPayment, error) {

	return p.db.FetchPayment(paymentHash)
}

// FailPayment transitions a payment into the Failed state, and records the
// reason the payment failed. After invoking this method, InitPayment should
// return nil on its next call for this payment hash, allowing the switch to
// make a subsequent payment.
//
// NOTE: This method will overwrite the failure reason if the payment is already
// failed.
func (p *controlTower) FailPayment(paymentHash lntypes.Hash,
	reason paymentsdb.FailureReason) error {

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
func (p *controlTower) FetchInFlightPayments() ([]*paymentsdb.MPPayment,
	error) {

	return p.db.FetchInFlightPayments()
}

// SubscribePayment subscribes to updates for the payment with the given hash. A
// first update with the current state of the payment is always sent out
// immediately.
func (p *controlTower) SubscribePayment(paymentHash lntypes.Hash) (
	ControlTowerSubscriber, error) {

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
	if !payment.Terminated() {
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

// SubscribeAllPayments subscribes to updates for all inflight payments. A first
// update with the current state of every inflight payment is always sent out
// immediately.
// Note: If payments are in-flight while starting a new subscription, the start
// of the payment stream could produce out-of-order and/or duplicate events. In
// order to get updates for every in-flight payment attempt make sure to
// subscribe to this method before initiating any payments.
func (p *controlTower) SubscribeAllPayments() (ControlTowerSubscriber, error) {
	subscriber := newControlTowerSubscriber()

	// Add the subscriber to the list before fetching in-flight payments, so
	// no events are missed. If a payment attempt update occurs after
	// appending and before fetching in-flight payments, an out-of-order
	// duplicate may be produced, because it is then fetched in below call
	// and notified through the subscription.
	p.subscribersMtx.Lock()
	p.subscribersAllPayments[p.subscriberIndex] = subscriber
	p.subscriberIndex++
	p.subscribersMtx.Unlock()

	log.Debugf("Scanning for inflight payments")
	inflightPayments, err := p.db.FetchInFlightPayments()
	if err != nil {
		return nil, err
	}
	log.Debugf("Scanning for inflight payments finished",
		len(inflightPayments))

	for index := range inflightPayments {
		// Always write current payment state to the channel.
		subscriber.queue.ChanIn() <- inflightPayments[index]
	}

	return subscriber, nil
}

// notifySubscribers sends a final payment event to all subscribers of this
// payment. The channel will be closed after this. Note that this function must
// be executed atomically (by means of a lock) with the database update to
// guarantee consistency of the notifications.
func (p *controlTower) notifySubscribers(paymentHash lntypes.Hash,
	event *paymentsdb.MPPayment) {

	// Get all subscribers for this payment.
	p.subscribersMtx.Lock()

	subscribersPaymentHash, ok := p.subscribers[paymentHash]
	if !ok && len(p.subscribersAllPayments) == 0 {
		p.subscribersMtx.Unlock()
		return
	}

	// If the payment reached a terminal state, the subscriber list can be
	// cleared. There won't be any more updates.
	terminal := event.Terminated()
	if terminal {
		delete(p.subscribers, paymentHash)
	}

	// Copy subscribers to all payments locally while holding the lock in
	// order to avoid concurrency issues while reading/writing the map.
	subscribersAllPayments := make(map[uint64]*controlTowerSubscriberImpl)
	for k, v := range p.subscribersAllPayments {
		subscribersAllPayments[k] = v
	}
	p.subscribersMtx.Unlock()

	// Notify all subscribers that subscribed to the current payment hash.
	for _, subscriber := range subscribersPaymentHash {
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

	// Notify all subscribers that subscribed to all payments.
	for key, subscriber := range subscribersAllPayments {
		select {
		case subscriber.queue.ChanIn() <- event:

		// If subscriber disappeared, remove it from the subscribers
		// list.
		case <-subscriber.quit:
			p.subscribersMtx.Lock()
			delete(p.subscribersAllPayments, key)
			p.subscribersMtx.Unlock()
		}
	}
}
