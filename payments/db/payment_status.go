package paymentsdb

import (
	"fmt"
)

// PaymentStatus represent current status of payment.
type PaymentStatus byte

const (
	// NOTE: PaymentStatus = 0 was previously used for status unknown and
	// is now deprecated.

	// StatusInitiated is the status where a payment has just been
	// initiated.
	StatusInitiated PaymentStatus = 1

	// StatusInFlight is the status where a payment has been initiated, but
	// a response has not been received.
	StatusInFlight PaymentStatus = 2

	// StatusSucceeded is the status where a payment has been initiated and
	// the payment was completed successfully.
	StatusSucceeded PaymentStatus = 3

	// StatusFailed is the status where a payment has been initiated and a
	// failure result has come back.
	StatusFailed PaymentStatus = 4
)

// errPaymentStatusUnknown is returned when a payment has an unknown status.
var errPaymentStatusUnknown = fmt.Errorf("unknown payment status")

// String returns readable representation of payment status.
func (ps PaymentStatus) String() string {
	switch ps {
	case StatusInitiated:
		return "Initiated"

	case StatusInFlight:
		return "In Flight"

	case StatusSucceeded:
		return "Succeeded"

	case StatusFailed:
		return "Failed"

	default:
		return "Unknown"
	}
}

// initializable returns an error to specify whether initiating the payment
// with its current status is allowed. A payment can only be initialized if it
// hasn't been created yet or already failed.
func (ps PaymentStatus) initializable() error {
	switch ps {
	// The payment has been created already. We will disallow creating it
	// again in case other goroutines have already been creating HTLCs for
	// it.
	case StatusInitiated:
		return ErrPaymentExists

	// We already have an InFlight payment on the network. We will disallow
	// any new payments.
	case StatusInFlight:
		return ErrPaymentInFlight

	// The payment has been attempted and is succeeded so we won't allow
	// creating it again.
	case StatusSucceeded:
		return ErrAlreadyPaid

	// We allow retrying failed payments.
	case StatusFailed:
		return nil

	default:
		return fmt.Errorf("%w: %v", ErrUnknownPaymentStatus,
			ps)
	}
}

// removable returns an error to specify whether deleting the payment with its
// current status is allowed. A payment cannot be safely deleted if it has
// inflight HTLCs.
func (ps PaymentStatus) removable() error {
	switch ps {
	// The payment has been created but has no HTLCs and can be removed.
	case StatusInitiated:
		return nil

	// There are still inflight HTLCs and the payment needs to wait for the
	// final outcomes.
	case StatusInFlight:
		return ErrPaymentInFlight

	// The payment has been attempted and is succeeded and is allowed to be
	// removed.
	case StatusSucceeded:
		return nil

	// Failed payments are allowed to be removed.
	case StatusFailed:
		return nil

	default:
		return fmt.Errorf("%w: %v", ErrUnknownPaymentStatus,
			ps)
	}
}

// updatable returns an error to specify whether the payment's HTLCs can be
// updated. A payment can update its HTLCs when it has inflight HTLCs.
func (ps PaymentStatus) updatable() error {
	switch ps {
	// Newly created payments can be updated.
	case StatusInitiated:
		return nil

	// Inflight payments can be updated.
	case StatusInFlight:
		return nil

	// If the payment has a terminal condition, we won't allow any updates.
	case StatusSucceeded:
		return ErrPaymentAlreadySucceeded

	case StatusFailed:
		return ErrPaymentAlreadyFailed

	default:
		return fmt.Errorf("%w: %v", ErrUnknownPaymentStatus,
			ps)
	}
}

// decidePaymentStatus uses the payment's DB state to determine a memory status
// that's used by the payment router to decide following actions.
// Together, we use four variables to determine the payment's status,
//   - inflight: whether there are any pending HTLCs.
//   - settled: whether any of the HTLCs has been settled.
//   - htlc failed: whether any of the HTLCs has been failed.
//   - payment failed: whether the payment has been marked as failed.
//
// Based on the above variables, we derive the status using the following
// table,
// | inflight | settled | htlc failed | payment failed |         status       |
// |:--------:|:-------:|:-----------:|:--------------:|:--------------------:|
// |   true   |   true  |     true    |      true      |    StatusInFlight    |
// |   true   |   true  |     true    |      false     |    StatusInFlight    |
// |   true   |   true  |     false   |      true      |    StatusInFlight    |
// |   true   |   true  |     false   |      false     |    StatusInFlight    |
// |   true   |   false |     true    |      true      |    StatusInFlight    |
// |   true   |   false |     true    |      false     |    StatusInFlight    |
// |   true   |   false |     false   |      true      |    StatusInFlight    |
// |   true   |   false |     false   |      false     |    StatusInFlight    |
// |   false  |   true  |     true    |      true      |    StatusSucceeded   |
// |   false  |   true  |     true    |      false     |    StatusSucceeded   |
// |   false  |   true  |     false   |      true      |    StatusSucceeded   |
// |   false  |   true  |     false   |      false     |    StatusSucceeded   |
// |   false  |   false |     true    |      true      |      StatusFailed    |
// |   false  |   false |     true    |      false     |    StatusInFlight    |
// |   false  |   false |     false   |      true      |      StatusFailed    |
// |   false  |   false |     false   |      false     |    StatusInitiated   |
//
// When `inflight`, `settled`, `htlc failed`, and `payment failed` are false,
// this indicates the payment is newly created and hasn't made any HTLCs yet.
// When `inflight` and `settled` are false, `htlc failed` is true yet `payment
// failed` is false, this indicates all the payment's HTLCs have occurred a
// temporarily failure and the payment is still in-flight.
func decidePaymentStatus(htlcs []HTLCAttempt,
	reason *FailureReason) (PaymentStatus, error) {

	var (
		inflight      bool
		htlcSettled   bool
		htlcFailed    bool
		paymentFailed bool
	)

	// If we have a failure reason, the payment is failed.
	if reason != nil {
		paymentFailed = true
	}

	// Go through all HTLCs for this payment, check whether we have any
	// settled HTLC, and any still in-flight.
	for _, h := range htlcs {
		if h.Failure != nil {
			htlcFailed = true
			continue
		}

		if h.Settle != nil {
			htlcSettled = true
			continue
		}

		// If any of the HTLCs are not failed nor settled, we
		// still have inflight HTLCs.
		inflight = true
	}

	// Use the DB state to determine the status of the payment.
	switch {
	// If we have inflight HTLCs, no matter we have settled or failed
	// HTLCs, or the payment failed, we still consider it inflight so we
	// inform upper systems to wait for the results.
	case inflight:
		return StatusInFlight, nil

	// If we have no in-flight HTLCs, and at least one of the HTLCs is
	// settled, the payment succeeded.
	//
	// NOTE: when reaching this case, paymentFailed could be true, which
	// means we have a conflicting state for this payment. We choose to
	// mark the payment as succeeded because it's the receiver's
	// responsibility to only settle the payment iff all HTLCs are
	// received.
	case htlcSettled:
		return StatusSucceeded, nil

	// If we have no in-flight HTLCs, and the payment failure is set, the
	// payment is considered failed.
	//
	// NOTE: when reaching this case, settled must be false.
	case paymentFailed:
		return StatusFailed, nil

	// If we have no in-flight HTLCs, yet the payment is NOT failed, it
	// means all the HTLCs are failed. In this case we can attempt more
	// HTLCs.
	//
	// NOTE: when reaching this case, both settled and paymentFailed must
	// be false.
	case htlcFailed:
		return StatusInFlight, nil

	// If none of the HTLCs is either settled or failed, and we have no
	// inflight HTLCs, this means the payment has no HTLCs created yet.
	//
	// NOTE: when reaching this case, both settled and paymentFailed must
	// be false.
	case !htlcFailed:
		return StatusInitiated, nil

	// Otherwise an impossible state is reached.
	//
	// NOTE: we should never end up here.
	default:
		log.Error("Impossible payment state reached")
		return 0, fmt.Errorf("%w: payment is corrupted",
			errPaymentStatusUnknown)
	}
}
