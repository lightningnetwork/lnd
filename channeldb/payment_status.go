package channeldb

import "fmt"

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
		return fmt.Errorf("%w: %v", ErrUnknownPaymentStatus, ps)
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
		return fmt.Errorf("%w: %v", ErrUnknownPaymentStatus, ps)
	}
}
