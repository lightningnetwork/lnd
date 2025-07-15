package paymentsdb

import (
	"context"

	"github.com/lightningnetwork/lnd/lntypes"
)

// PaymentDB is the interface that represents the underlying payments database.
type PaymentDB interface {
	PaymentReader
	PaymentWriter
	Sequencer
}

// PaymentReader is the interface that reads from the payments database.
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

// PaymentWriter is the interface that writes to the payments database.
type PaymentWriter interface {
	// DeletePayment deletes a payment from the DB given its payment hash.
	DeletePayment(paymentHash lntypes.Hash, failedAttemptsOnly bool) error

	// DeletePayments deletes all payments from the DB given the specified
	// flags.
	DeletePayments(failedOnly, failedAttemptsOnly bool) (int, error)

	// DeleteFailedAttempts removes all failed HTLCs from the db. It should
	// be called for a given payment whenever all inflight htlcs are
	// completed, and the payment has reached a final settled state.
	DeleteFailedAttempts(lntypes.Hash) error

	PaymentControl
}

// Sequencer emits sequence numbers for locally initiated HTLCs. These are
// only used internally for tracking pending payments, however they must be
// unique in order to avoid circuit key collision in the circuit map.
type Sequencer interface {
	// NextID returns a unique sequence number for each invocation.
	NextID() (uint64, error)
}

// PaymentControl is the interface that controls the payment lifecycle.
type PaymentControl interface {
	// This method checks that no succeeded payment exist for this payment
	// hash.
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
}
