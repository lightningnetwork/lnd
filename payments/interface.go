package payments

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
)

// PaymentDB defines methods to handle payments on disk. This interface is...
//
// TODO(yy): embed this interface in ControlTower.
type PaymentDB interface {
	// This method checks that no succeeded payment exist for this payment
	// hash.
	InitPayment(lntypes.Hash, *channeldb.PaymentCreationInfo) error

	// DeleteFailedAttempts removes all failed HTLCs from the db. It should
	// be called for a given payment whenever all inflight htlcs are
	// completed, and the payment has reached a final settled state.
	DeleteFailedAttempts(lntypes.Hash) error

	// RegisterAttempt atomically records the provided HTLCAttemptInfo.
	RegisterAttempt(lntypes.Hash, *channeldb.HTLCAttemptInfo) error

	// SettleAttempt marks the given attempt settled with the preimage. If
	// this is a multi shard payment, this might implicitly mean the the
	// full payment succeeded.
	//
	// After invoking this method, InitPayment should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. The provided preimage is atomically saved to the DB
	// for record keeping.
	SettleAttempt(lntypes.Hash, uint64, *channeldb.HTLCSettleInfo) (
		*channeldb.HTLCAttempt, error)

	// FailAttempt marks the given payment attempt failed.
	FailAttempt(lntypes.Hash, uint64, *channeldb.HTLCFailInfo) (
		*channeldb.HTLCAttempt, error)

	// FetchPayment fetches the payment corresponding to the given payment
	// hash.
	FetchPayment(paymentHash lntypes.Hash) (*channeldb.MPPayment, error)

	// FailPayment transitions a payment into the Failed state, and records
	// the ultimate reason the payment failed. Note that this should only
	// be called when all active attempts are already failed. After
	// invoking this method, InitPayment should return nil on its next call
	// for this payment hash, allowing the user to make a subsequent
	// payment.
	FailPayment(lntypes.Hash, channeldb.FailureReason) error

	// FetchInFlightPayments returns all payments with status InFlight.
	FetchInFlightPayments() ([]*channeldb.MPPayment, error)
}
