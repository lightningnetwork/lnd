package payments

import (
	"context"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
)

// PaymentDB is the interface that represents the underlying payments database.
//
//nolint:interfacebloat
type PaymentDB interface {
	// QueryPayments queries the payments database and should support
	// pagination.
	QueryPayments(ctx context.Context,
		query channeldb.PaymentsQuery) (channeldb.PaymentsResponse,
		error)

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
	InitPayment(lntypes.Hash, *channeldb.PaymentCreationInfo) error

	// DeleteFailedAttempts removes all failed HTLCs from the db. It should
	// be called for a given payment whenever all inflight htlcs are
	// completed, and the payment has reached a final settled state.
	DeleteFailedAttempts(lntypes.Hash) error

	// RegisterAttempt atomically records the provided HTLCAttemptInfo.
	RegisterAttempt(lntypes.Hash,
		*channeldb.HTLCAttemptInfo) (*channeldb.MPPayment, error)

	// SettleAttempt marks the given attempt settled with the preimage. If
	// this is a multi shard payment, this might implicitly mean the
	// full payment succeeded.
	//
	// After invoking this method, InitPayment should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. The provided preimage is atomically saved to the DB
	// for record keeping.
	SettleAttempt(lntypes.Hash, uint64, *channeldb.HTLCSettleInfo) (
		*channeldb.MPPayment, error)

	// FailAttempt marks the given payment attempt failed.
	FailAttempt(lntypes.Hash, uint64, *channeldb.HTLCFailInfo) (
		*channeldb.MPPayment, error)

	// FetchPayment fetches the payment corresponding to the given payment
	// hash.
	FetchPayment(paymentHash lntypes.Hash) (*channeldb.MPPayment, error)

	// Fail transitions a payment into the Failed state, and records
	// the ultimate reason the payment failed. Note that this should only
	// be called when all active attempts are already failed. After
	// invoking this method, InitPayment should return nil on its next call
	// for this payment hash, allowing the user to make a subsequent
	// payment.
	Fail(lntypes.Hash, channeldb.FailureReason) (*channeldb.MPPayment,
		error)

	// FetchInFlightPayments returns all payments with status InFlight.
	FetchInFlightPayments() ([]*channeldb.MPPayment, error)
}
