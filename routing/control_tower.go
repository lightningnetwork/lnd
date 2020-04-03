package routing

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
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
	SettleAttempt(lntypes.Hash, uint64, *channeldb.HTLCSettleInfo) error

	// FailAttempt marks the given payment attempt failed.
	FailAttempt(lntypes.Hash, uint64, *channeldb.HTLCFailInfo) error

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
	FetchInFlightPayments() ([]*channeldb.InFlightPayment, error)

	// SubscribePayment subscribes to updates for the payment with the given
	// hash. It returns a boolean indicating whether the payment is still in
	// flight and a channel that provides the final outcome of the payment.
	SubscribePayment(paymentHash lntypes.Hash) (bool, chan PaymentResult,
		error)
}

// PaymentResult is the struct describing the events received by payment
// subscribers.
type PaymentResult struct {
	// Success indicates whether the payment was successful.
	Success bool

	// Preimage is the preimage of a successful payment. This serves as a
	// proof of payment. It is only set for successful payments.
	Preimage lntypes.Preimage

	// FailureReason is a failure reason code indicating the reason the
	// payment failed. It is only set for failed payments.
	FailureReason channeldb.FailureReason

	// HTLCs is a list of HTLCs that have been attempted in order to settle
	// the payment.
	HTLCs []channeldb.HTLCAttempt
}

// controlTower is persistent implementation of ControlTower to restrict
// double payment sending.
type controlTower struct {
	db *channeldb.PaymentControl

	subscribers    map[lntypes.Hash][]chan PaymentResult
	subscribersMtx sync.Mutex
}

// NewControlTower creates a new instance of the controlTower.
func NewControlTower(db *channeldb.PaymentControl) ControlTower {
	return &controlTower{
		db:          db,
		subscribers: make(map[lntypes.Hash][]chan PaymentResult),
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

	return p.db.RegisterAttempt(paymentHash, attempt)
}

// SettleAttempt marks the given attempt settled with the preimage. If
// this is a multi shard payment, this might implicitly mean the the
// full payment succeeded.
func (p *controlTower) SettleAttempt(paymentHash lntypes.Hash,
	attemptID uint64, settleInfo *channeldb.HTLCSettleInfo) error {

	payment, err := p.db.SettleAttempt(paymentHash, attemptID, settleInfo)
	if err != nil {
		return err
	}

	// Notify subscribers of success event.
	p.notifyFinalEvent(
		paymentHash, createSuccessResult(payment.HTLCs),
	)

	return nil
}

// FailAttempt marks the given payment attempt failed.
func (p *controlTower) FailAttempt(paymentHash lntypes.Hash,
	attemptID uint64, failInfo *channeldb.HTLCFailInfo) error {

	return p.db.FailAttempt(paymentHash, attemptID, failInfo)
}

// FetchPayment fetches the payment corresponding to the given payment hash.
func (p *controlTower) FetchPayment(paymentHash lntypes.Hash) (
	*channeldb.MPPayment, error) {

	return p.db.FetchPayment(paymentHash)
}

// createSuccessResult creates a success result to send to subscribers.
func createSuccessResult(htlcs []channeldb.HTLCAttempt) *PaymentResult {
	// Extract any preimage from the list of HTLCs.
	var preimage lntypes.Preimage
	for _, htlc := range htlcs {
		if htlc.Settle != nil {
			preimage = htlc.Settle.Preimage
			break
		}
	}

	return &PaymentResult{
		Success:  true,
		Preimage: preimage,
		HTLCs:    htlcs,
	}
}

// createFailResult creates a failed result to send to subscribers.
func createFailedResult(htlcs []channeldb.HTLCAttempt,
	reason channeldb.FailureReason) *PaymentResult {

	return &PaymentResult{
		Success:       false,
		FailureReason: reason,
		HTLCs:         htlcs,
	}
}

// Fail transitions a payment into the Failed state, and records the reason the
// payment failed. After invoking this method, InitPayment should return nil on
// its next call for this payment hash, allowing the switch to make a
// subsequent payment.
func (p *controlTower) Fail(paymentHash lntypes.Hash,
	reason channeldb.FailureReason) error {

	payment, err := p.db.Fail(paymentHash, reason)
	if err != nil {
		return err
	}

	// Notify subscribers of fail event.
	p.notifyFinalEvent(
		paymentHash, createFailedResult(
			payment.HTLCs, reason,
		),
	)

	return nil
}

// FetchInFlightPayments returns all payments with status InFlight.
func (p *controlTower) FetchInFlightPayments() ([]*channeldb.InFlightPayment, error) {
	return p.db.FetchInFlightPayments()
}

// SubscribePayment subscribes to updates for the payment with the given hash.
// It returns a boolean indicating whether the payment is still in flight and a
// channel that provides the final outcome of the payment.
func (p *controlTower) SubscribePayment(paymentHash lntypes.Hash) (
	bool, chan PaymentResult, error) {

	// Create a channel with buffer size 1. For every payment there will be
	// exactly one event sent.
	c := make(chan PaymentResult, 1)

	// Take lock before querying the db to prevent this scenario:
	// FetchPayment returns us an in-flight state -> payment succeeds, but
	// there is no subscriber to notify yet -> we add ourselves as a
	// subscriber -> ... we will never receive a notification.
	p.subscribersMtx.Lock()
	defer p.subscribersMtx.Unlock()

	payment, err := p.db.FetchPayment(paymentHash)
	if err != nil {
		return false, nil, err
	}

	var event PaymentResult

	switch payment.Status {

	// Payment is currently in flight. Register this subscriber and
	// return without writing a result to the channel yet.
	case channeldb.StatusInFlight:
		p.subscribers[paymentHash] = append(
			p.subscribers[paymentHash], c,
		)

		return true, c, nil

	// Payment already succeeded. It is not necessary to register as
	// a subscriber, because we can send the result on the channel
	// immediately.
	case channeldb.StatusSucceeded:
		event = *createSuccessResult(payment.HTLCs)

	// Payment already failed. It is not necessary to register as a
	// subscriber, because we can send the result on the channel
	// immediately.
	case channeldb.StatusFailed:
		event = *createFailedResult(
			payment.HTLCs, *payment.FailureReason,
		)

	default:
		return false, nil, errors.New("unknown payment status")
	}

	// Write immediate result to the channel.
	c <- event
	close(c)

	return false, c, nil
}

// notifyFinalEvent sends a final payment event to all subscribers of this
// payment. The channel will be closed after this.
func (p *controlTower) notifyFinalEvent(paymentHash lntypes.Hash,
	event *PaymentResult) {

	// Get all subscribers for this hash. As there is only a single outcome,
	// the subscriber list can be cleared.
	p.subscribersMtx.Lock()
	list, ok := p.subscribers[paymentHash]
	if !ok {
		p.subscribersMtx.Unlock()
		return
	}
	delete(p.subscribers, paymentHash)
	p.subscribersMtx.Unlock()

	// Notify all subscribers of the event. The subscriber channel is
	// buffered, so it cannot block here.
	for _, subscriber := range list {
		subscriber <- *event
		close(subscriber)
	}
}
