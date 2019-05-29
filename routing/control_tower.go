package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/lightningnetwork/lnd/lntypes"
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
}

// controlTower is persistent implementation of ControlTower to restrict
// double payment sending.
type controlTower struct {
	db *channeldb.PaymentControl
}

// NewControlTower creates a new instance of the controlTower.
func NewControlTower(db *channeldb.PaymentControl) ControlTower {
	return &controlTower{
		db: db,
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

	return p.db.Success(paymentHash, preimage)
}

// Fail transitions a payment into the Failed state, and records the reason the
// payment failed. After invoking this method, InitPayment should return nil on
// its next call for this payment hash, allowing the switch to make a
// subsequent payment.
func (p *controlTower) Fail(paymentHash lntypes.Hash,
	reason channeldb.FailureReason) error {

	return p.db.Fail(paymentHash, reason)
}

// FetchInFlightPayments returns all payments with status InFlight.
func (p *controlTower) FetchInFlightPayments() ([]*channeldb.InFlightPayment, error) {
	return p.db.FetchInFlightPayments()
}
