package channeldb

import (
	"errors"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrAlreadyPaid signals we have already paid this payment hash.
	ErrAlreadyPaid = errors.New("invoice is already paid")

	// ErrPaymentInFlight signals that payment for this payment hash is
	// already "in flight" on the network.
	ErrPaymentInFlight = errors.New("payment is in transition")

	// ErrPaymentNotInitiated is returned  if payment wasn't initiated in
	// switch.
	ErrPaymentNotInitiated = errors.New("payment isn't initiated")

	// ErrPaymentAlreadyCompleted is returned in the event we attempt to
	// recomplete a completed payment.
	ErrPaymentAlreadyCompleted = errors.New("payment is already completed")

	// ErrUnknownPaymentStatus is returned when we do not recognize the
	// existing state of a payment.
	ErrUnknownPaymentStatus = errors.New("unknown payment status")
)

// ControlTower tracks all outgoing payments made by the switch, whose primary
// purpose is to prevent duplicate payments to the same payment hash. In
// production, a persistent implementation is preferred so that tracking can
// survive across restarts. Payments are transition through various payment
// states, and the ControlTower interface provides access to driving the state
// transitions.
type ControlTower interface {
	// ClearForTakeoff atomically checks that no inflight or completed
	// payments exist for this payment hash. If none are found, this method
	// atomically transitions the status for this payment hash as InFlight.
	ClearForTakeoff(htlc *lnwire.UpdateAddHTLC) error

	// Success transitions an InFlight payment into a Completed payment.
	// After invoking this method, ClearForTakeoff should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash.
	Success(paymentHash [32]byte) error

	// Fail transitions an InFlight payment into a Grounded Payment. After
	// invoking this method, ClearForTakeoff should return nil on its next
	// call for this payment hash, allowing the switch to make a subsequent
	// payment.
	Fail(paymentHash [32]byte) error
}

// paymentControl is persistent implementation of ControlTower to restrict
// double payment sending.
type paymentControl struct {
	db *DB
}

// NewPaymentControl creates a new instance of the paymentControl.
func NewPaymentControl(db *DB) ControlTower {
	return &paymentControl{
		db: db,
	}
}

// ClearForTakeoff checks that we don't already have an InFlight or Completed
// payment identified by the same payment hash.
func (p *paymentControl) ClearForTakeoff(htlc *lnwire.UpdateAddHTLC) error {
	var takeoffErr error
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := fetchPaymentBucket(tx, htlc.PaymentHash)
		if err != nil {
			return err
		}

		// Get the existing status of this payment, if any.
		paymentStatus := fetchPaymentStatus(bucket)

		// Reset the takeoff error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		takeoffErr = nil

		switch paymentStatus {

		// It is safe to reattempt a payment if we know that we haven't
		// left one in flight. Since this one is grounded or failed,
		// transition the payment status to InFlight to prevent others.
		case StatusGrounded:
			return bucket.Put(paymentStatusKey, StatusInFlight.Bytes())

		// We already have an InFlight payment on the network. We will
		// disallow any more payment until a response is received.
		case StatusInFlight:
			takeoffErr = ErrPaymentInFlight

		// We've already completed a payment to this payment hash,
		// forbid the switch from sending another.
		case StatusCompleted:
			takeoffErr = ErrAlreadyPaid

		default:
			takeoffErr = ErrUnknownPaymentStatus
		}

		return nil
	})
	if err != nil {
		return err
	}

	return takeoffErr
}

// Success transitions an InFlight payment to Completed, otherwise it returns an
// error. After calling Success, ClearForTakeoff should prevent any further
// attempts for the same payment hash.
func (p *paymentControl) Success(paymentHash [32]byte) error {
	var updateErr error
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := fetchPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		// Get the existing status, if any.
		paymentStatus := fetchPaymentStatus(bucket)

		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil

		switch {

		// Our records show the payment as still being grounded,
		// meaning it never should have left the switch.
		case paymentStatus == StatusGrounded:
			updateErr = ErrPaymentNotInitiated

		// A successful response was received for an InFlight payment,
		// mark it as completed to prevent sending to this payment hash
		// again.
		case paymentStatus == StatusInFlight:
			return bucket.Put(paymentStatusKey, StatusCompleted.Bytes())

		// The payment was completed previously, alert the caller that
		// this may be a duplicate call.
		case paymentStatus == StatusCompleted:
			updateErr = ErrPaymentAlreadyCompleted

		default:
			updateErr = ErrUnknownPaymentStatus
		}

		return nil
	})
	if err != nil {
		return err
	}

	return updateErr
}

// Fail transitions an InFlight payment to Grounded, otherwise it returns an
// error. After calling Fail, ClearForTakeoff should fail any further attempts
// for the same payment hash.
func (p *paymentControl) Fail(paymentHash [32]byte) error {
	var updateErr error
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := fetchPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		paymentStatus := fetchPaymentStatus(bucket)

		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil

		switch {

		// Our records show the payment as still being grounded,
		// meaning it never should have left the switch.
		case paymentStatus == StatusGrounded:
			updateErr = ErrPaymentNotInitiated

		// A failed response was received for an InFlight payment, mark
		// it as Failed to allow subsequent attempts.
		case paymentStatus == StatusInFlight:
			return bucket.Put(paymentStatusKey, StatusGrounded.Bytes())

		// The payment was completed previously, and we are now
		// reporting that it has failed. Leave the status as completed,
		// but alert the user that something is wrong.
		case paymentStatus == StatusCompleted:
			updateErr = ErrPaymentAlreadyCompleted

		default:
			updateErr = ErrUnknownPaymentStatus
		}

		return nil
	})
	if err != nil {
		return err
	}

	return updateErr
}

// fetchPaymentBucket fetches or creates the sub-bucket assigned to this
// payment hash.
func fetchPaymentBucket(tx *bbolt.Tx, paymentHash lntypes.Hash) (
	*bbolt.Bucket, error) {

	payments, err := tx.CreateBucketIfNotExists(paymentsRootBucket)
	if err != nil {
		return nil, err
	}

	return payments.CreateBucketIfNotExists(paymentHash[:])
}

// fetchPaymentStatus fetches the payment status from the bucket.  If the
// status isn't found, it will default to "StatusGrounded".
func fetchPaymentStatus(bucket *bbolt.Bucket) PaymentStatus {
	// The default status for all payments that aren't recorded in
	// database.
	var paymentStatus = StatusGrounded

	paymentStatusBytes := bucket.Get(paymentStatusKey)
	if paymentStatusBytes != nil {
		paymentStatus.FromBytes(paymentStatusBytes)
	}

	return paymentStatus
}
