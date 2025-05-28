package payments

import (
	"context"

	"github.com/lightningnetwork/lnd/lntypes"
)

// PaymentDB is the interface that represents the underlying payments database.
type PaymentDB interface {
	// QueryPayments queries the payments database and should support
	// pagination.
	QueryPayments(ctx context.Context,
		query Query) (Response, error)

	// DeletePayment deletes a payment from the DB given its payment hash.
	DeletePayment(paymentHash lntypes.Hash, failedHtlcsOnly bool) error

	// DeletePayments deletes all payments from the DB given the specified
	// flags.
	//
	// TODO(ziggie): use DeletePayment here as well to make the interface
	// more leaner.
	DeletePayments(failedOnly, failedHtlcsOnly bool) (int, error)

	// This method checks that no succeeded payment exist for this payment
	// hash.
	InitPayment(lntypes.Hash, *PaymentCreationInfo) error

	// DeleteFailedAttempts removes all failed HTLCs from the db. It should
	// be called for a given payment whenever all inflight htlcs are
	// completed, and the payment has reached a final settled state.
	DeleteFailedAttempts(lntypes.Hash) error

	// RegisterAttempt atomically records the provided HTLCAttemptInfo.
	RegisterAttempt(lntypes.Hash, *HTLCAttemptInfo) (*MPPayment, error)

	// SettleAttempt marks the given attempt settled with the preimage. If
	// this is a multi shard payment, this might implicitly mean the the
	// full payment succeeded.
	//
	// After invoking this method, InitPayment should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. The provided preimage is atomically saved to the DB
	// for record keeping.
	SettleAttempt(lntypes.Hash, uint64, *HTLCSettleInfo) (
		*MPPayment, error)

	// FailAttempt marks the given payment attempt failed.
	FailAttempt(lntypes.Hash, uint64, *HTLCFailInfo) (
		*MPPayment, error)

	// FetchPayment fetches the payment corresponding to the given payment
	// hash.
	FetchPayment(paymentHash lntypes.Hash) (*MPPayment, error)

	// FailPayment transitions a payment into the Failed state, and records
	// the ultimate reason the payment failed. Note that this should only
	// be called when all active attempts are already failed. After
	// invoking this method, InitPayment should return nil on its next call
	// for this payment hash, allowing the user to make a subsequent
	// payment.
	FailPayment(lntypes.Hash, FailureReason) (*MPPayment, error)

	// FetchInFlightPayments returns all payments with status InFlight.
	FetchInFlightPayments() ([]*MPPayment, error)
}

// Payment is an interface which defines the core interface for managing a
// payment's lifecycle.
type Payment interface {
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
