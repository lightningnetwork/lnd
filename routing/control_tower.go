package routing

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
)

// ControlTower tracks all outgoing payments made, whose primary purpose is to
// prevent duplicate payments to the same payment hash. In production, a
// persistent implementation is preferred so that tracking can survive across
// restarts. Payments are transitioned through various payment states, and the
// ControlTower interface provides access to driving the state transitions.
type ControlTower interface {
	// InitPayment atomically moves the payment into the InFlight state.
	// This method checks that no suceeded payment exist for this payment
	// hash.
	InitPayment(lntypes.Hash, *channeldb.PaymentCreationInfo) error

	// RegisterAttempt atomically records the provided PaymentAttemptInfo.
	RegisterAttempt(lntypes.Hash, *channeldb.PaymentAttemptInfo) error

	// Success transitions a payment into the Succeeded state. After
	// invoking this method, InitPayment should always return an error to
	// prevent us from making duplicate payments to the same payment hash.
	// The provided preimage is atomically saved to the DB for record
	// keeping.
	Success(lntypes.Hash, lntypes.Preimage) error

	// Fail transitions a payment into the Failed state, and records the
	// reason the payment failed. After invoking this method, InitPayment
	// should return nil on its next call for this payment hash, allowing
	// the switch to make a subsequent payment.
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

	// Route is the (last) route attempted to send the HTLC. It is only set
	// for successful payments.
	Route *route.Route

	// PaymentPreimage is the preimage of a successful payment. This serves
	// as a proof of payment. It is only set for successful payments.
	Preimage lntypes.Preimage

	// Failure is a failure reason code indicating the reason the payment
	// failed. It is only set for failed payments.
	FailureReason channeldb.FailureReason
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

// RegisterAttempt atomically records the provided PaymentAttemptInfo to the
// DB.
func (p *controlTower) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *channeldb.PaymentAttemptInfo) error {

	return p.db.RegisterAttempt(paymentHash, attempt)
}

// Success transitions a payment into the Succeeded state. After invoking this
// method, InitPayment should always return an error to prevent us from making
// duplicate payments to the same payment hash. The provided preimage is
// atomically saved to the DB for record keeping.
func (p *controlTower) Success(paymentHash lntypes.Hash,
	preimage lntypes.Preimage) error {

	route, err := p.db.Success(paymentHash, preimage)
	if err != nil {
		return err
	}

	// Notify subscribers of success event.
	p.notifyFinalEvent(
		paymentHash, createSuccessResult(route, preimage),
	)

	return nil
}

// createSuccessResult creates a success result to send to subscribers.
func createSuccessResult(rt *route.Route,
	preimage lntypes.Preimage) *PaymentResult {

	return &PaymentResult{
		Success:  true,
		Preimage: preimage,
		Route:    rt,
	}
}

// createFailResult creates a failed result to send to subscribers.
func createFailedResult(rt *route.Route,
	reason channeldb.FailureReason) *PaymentResult {

	result := &PaymentResult{
		Success:       false,
		FailureReason: reason,
	}

	// In case of incorrect payment details, set the route. This can be used
	// for probing and to extract a fee estimate from the route.
	if reason == channeldb.FailureReasonIncorrectPaymentDetails {
		result.Route = rt
	}

	return result
}

// Fail transitions a payment into the Failed state, and records the reason the
// payment failed. After invoking this method, InitPayment should return nil on
// its next call for this payment hash, allowing the switch to make a
// subsequent payment.
func (p *controlTower) Fail(paymentHash lntypes.Hash,
	reason channeldb.FailureReason) error {

	route, err := p.db.Fail(paymentHash, reason)
	if err != nil {
		return err
	}

	// Notify subscribers of fail event.
	p.notifyFinalEvent(
		paymentHash, createFailedResult(route, reason),
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
		event = *createSuccessResult(
			&payment.Attempt.Route, *payment.PaymentPreimage,
		)

	// Payment already failed. It is not necessary to register as a
	// subscriber, because we can send the result on the channel
	// immediately.
	case channeldb.StatusFailed:
		var route *route.Route
		if payment.Attempt != nil {
			route = &payment.Attempt.Route
		}
		event = *createFailedResult(
			route, *payment.Failure,
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
