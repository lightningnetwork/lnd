package payments

import "github.com/lightningnetwork/lnd/lntypes"

// PaymentDB is the database that stores the information about payments.
type PaymentDB interface {
	InitPayment(paymentHash lntypes.Hash,
		info *PaymentCreationInfo) error

	DeleteFailedAttempts(hash lntypes.Hash) error

	RegisterAttempt(paymentHash lntypes.Hash,
		attempt *HTLCAttemptInfo) (*MPPayment, error)

	SettleAttempt(hash lntypes.Hash,
		attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error)

	FailAttempt(hash lntypes.Hash,
		attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error)

	FetchPayment(paymentHash lntypes.Hash) (
		*MPPayment, error)

	FetchInFlightPayments() ([]*MPPayment, error)

	Fail(paymentHash lntypes.Hash,
		reason FailureReason) (*MPPayment, error)

	// DeletePayment(paymentHash lntypes.Hash,
	//      failedHtlcsOnly bool) error

	// DeletePayments(failedOnly, failedHtlcsOnly bool) error
}

// DBMPPayment is an interface derived from channeldb.MPPayment that is used by
// the payment lifecycle.
type dBMPPayment interface {
	// GetState returns the current state of the payment.
	GetState() *MPPaymentState

	// Terminated returns true if the payment is in a final state.
	Terminated() bool

	// GetStatus returns the current status of the payment.
	GetStatus() PaymentStatus

	// NeedWaitAttempts specifies whether the payment needs to wait for the
	// outcome of an attempt.
	NeedWaitAttempts() (bool, error)

	// GetHTLCs returns all HTLCs of this payment.
	GetHTLCs() []HTLCAttempt

	// InFlightHTLCs returns all HTLCs that are in flight.
	InFlightHTLCs() []HTLCAttempt

	// AllowMoreAttempts is used to decide whether we can safely attempt
	// more HTLCs for a given payment state. Return an error if the payment
	// is in an unexpected state.
	AllowMoreAttempts() (bool, error)

	// TerminalInfo returns the settled HTLC attempt or the payment's
	// failure reason.
	TerminalInfo() (*HTLCAttempt, *FailureReason)
}
