package paymentsdb

import (
	"context"

	"github.com/lightningnetwork/lnd/lntypes"
)

// DB represents the interface to the underlying payments database.
type DB interface {
	PaymentReader
	PaymentWriter
}

// PaymentReader represents the interface to read operations from the payments
// database.
type PaymentReader interface {
	// QueryPayments queries the payments database and should support
	// pagination.
	QueryPayments(ctx context.Context, query Query) (Response, error)

	// FetchPayment fetches the payment corresponding to the given payment
	// hash.
	FetchPayment(paymentHash lntypes.Hash) (*MPPayment, error)

	// FetchInFlightPayments returns all payments with status InFlight.
	FetchInFlightPayments() ([]*MPPayment, error)
}

// PaymentWriter represents the interface to write operations to the payments
// database.
type PaymentWriter interface {
	// DeletePayment deletes a payment from the DB given its payment hash.
	DeletePayment(paymentHash lntypes.Hash, failedAttemptsOnly bool) error

	// DeletePayments deletes all payments from the DB given the specified
	// flags.
	DeletePayments(failedOnly, failedAttemptsOnly bool) (int, error)

	PaymentControl
}

// PaymentControl represents the interface to control the payment lifecycle and
// its database operations. This interface represents the control flow of how
// a payment should be handled in the database. They are not just writing
// operations but they inherently represent the flow of a payment. The methods
// are called in the following order.
//
// 1. InitPayment.
// 2. RegisterAttempt (a payment can have multiple attempts).
// 3. SettleAttempt or FailAttempt (attempts can also fail as long as the
// sending amount will be eventually settled).
// 4. Payment succeeds or "Fail" is called.
// 5. DeleteFailedAttempts is called which will delete all failed attempts
// for a payment to clean up the database.
type PaymentControl interface {
	// InitPayment checks that no other payment with the same payment hash
	// exists in the database before creating a new payment. However, it
	// should allow the user making a subsequent payment if the payment is
	// in a Failed state.
	InitPayment(lntypes.Hash, *PaymentCreationInfo) error

	// RegisterAttempt atomically records the provided HTLCAttemptInfo.
	RegisterAttempt(lntypes.Hash, *HTLCAttemptInfo) (*MPPayment, error)

	// SettleAttempt marks the given attempt settled with the preimage. If
	// this is a multi shard payment, this might implicitly mean the
	// full payment succeeded.
	//
	// After invoking this method, InitPayment should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. The provided preimage is atomically saved to the DB
	// for record keeping.
	SettleAttempt(lntypes.Hash, uint64, *HTLCSettleInfo) (*MPPayment, error)

	// FailAttempt marks the given payment attempt failed.
	FailAttempt(lntypes.Hash, uint64, *HTLCFailInfo) (*MPPayment, error)

	// Fail transitions a payment into the Failed state, and records
	// the ultimate reason the payment failed. Note that this should only
	// be called when all active attempts are already failed. After
	// invoking this method, InitPayment should return nil on its next call
	// for this payment hash, allowing the user to make a subsequent
	// payment.
	Fail(lntypes.Hash, FailureReason) (*MPPayment, error)

	// DeleteFailedAttempts removes all failed HTLCs from the db. It should
	// be called for a given payment whenever all inflight htlcs are
	// completed, and the payment has reached a final terminal state.
	DeleteFailedAttempts(lntypes.Hash) error
}

// DBMPPayment is an interface that represents the payment state during a
// payment lifecycle.
type DBMPPayment interface {
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
