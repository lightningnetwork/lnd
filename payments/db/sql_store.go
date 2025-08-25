package paymentsdb

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sqldb"
)

// SQLQueries is a subset of the sqlc.Querier interface that can be used to
// execute queries against the SQL payments tables.
//
//nolint:ll,interfacebloat
type SQLQueries interface {
}

// BatchedSQLQueries is a version of the SQLQueries that's capable
// of batched database operations.
type BatchedSQLQueries interface {
	SQLQueries
	sqldb.BatchedTx[SQLQueries]
}

// SQLStore represents a storage backend.
type SQLStore struct {
	cfg *SQLStoreConfig
	db  BatchedSQLQueries

	// keepFailedPaymentAttempts is a flag that indicates whether we should
	// keep failed payment attempts in the database.
	keepFailedPaymentAttempts bool
}

// A compile-time constraint to ensure SQLStore implements DB.
var _ DB = (*SQLStore)(nil)

// SQLStoreConfig holds the configuration for the SQLStore.
type SQLStoreConfig struct {
	// QueryConfig holds configuration values for SQL queries.
	QueryCfg *sqldb.QueryConfig
}

// NewSQLStore creates a new SQLStore instance given a open
// BatchedSQLPaymentsQueries storage backend.
func NewSQLStore(cfg *SQLStoreConfig, db BatchedSQLQueries,
	options ...StoreOptionModifier) (*SQLStore, error) {

	opts := DefaultOptions()
	for _, applyOption := range options {
		applyOption(opts)
	}

	if opts.NoMigration {
		return nil, fmt.Errorf("the NoMigration option is not yet " +
			"supported for SQL stores")
	}

	return &SQLStore{
		cfg:                       cfg,
		db:                        db,
		keepFailedPaymentAttempts: opts.KeepFailedPaymentAttempts,
	}, nil
}

// QueryPayments queries the payments from the database.
//
// This is part of the DB interface.
func (s *SQLStore) QueryPayments(ctx context.Context,
	query Query) (Response, error) {

	return Response{}, nil
}

// FetchPayment fetches a payment from the database given its payment hash.
//
// This is part of the DB interface.
func (s *SQLStore) FetchPayment(paymentHash lntypes.Hash) (*MPPayment, error) {
	return nil, nil
}

// FetchInFlightPayments fetches all payments which are INFLIGHT.
//
// This is part of the DB interface.
func (s *SQLStore) FetchInFlightPayments() ([]*MPPayment, error) {
	return nil, nil
}

// DeletePayment deletes a payment from the database given its payment hash.
// It will only delete the failed attempts if the failedAttemptsOnly flag is
// set.
//
// This is part of the DB interface.
func (s *SQLStore) DeletePayment(paymentHash lntypes.Hash,
	failedAttemptsOnly bool) error {

	return nil
}

// DeletePayments deletes all completed and failed payments from the DB. If
// failedOnly is set, only failed payments will be considered for deletion. If
// failedHtlcsOnly is set, the payment itself won't be deleted, only failed HTLC
// attempts. The method returns the number of deleted payments, which is always
// 0 if failedHtlcsOnly is set.
//
// TODO(ziggie): Consider doing the deletion in a background job so we do not
// interfere with the main task of LND.
//
// This is part of the DB interface.
func (s *SQLStore) DeletePayments(failedOnly, failedAttemptsOnly bool) (int,
	error) {

	return 0, nil
}

// InitPayment checks that no other payment with the same payment hash
// exists in the database before creating a new payment. When this
// method returns successfully, the payment is guaranteed to be in the
// InFlight state.
//
// This is part of the DB interface.
func (s *SQLStore) InitPayment(paymentHash lntypes.Hash,
	creationInfo *PaymentCreationInfo) error {

	return nil
}

// insertNewPayment inserts a new payment into the database.
func (s *SQLStore) insertNewPayment(ctx context.Context, db SQLQueries,
	paymentHash lntypes.Hash, creationInfo *PaymentCreationInfo) error {

	return nil
}

// RegisterAttempt atomically records the provided HTLCAttemptInfo.
//
// This is part of the DB interface.
func (s *SQLStore) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *HTLCAttemptInfo) (*MPPayment, error) {

	return nil, nil
}

// SettleAttempt marks the given attempt settled with the preimage. If
// this is a multi shard payment, this might implicitly mean the
// full payment succeeded.
//
// This is part of the DB interface.
func (s *SQLStore) SettleAttempt(paymentHash lntypes.Hash,
	attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error) {

	return nil, nil
}

// FailAttempt marks the given payment attempt failed.
//
// This is part of the DB interface.
func (s *SQLStore) FailAttempt(paymentHash lntypes.Hash,
	attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error) {

	return nil, nil
}

// Fail transitions a payment into the Failed state, and records
// the ultimate reason the payment failed. Note that this should only
// be called when all active attempts are already failed. After
// invoking this method, InitPayment should return nil on its next call
// for this payment hash, allowing the user to make a subsequent
// payment.
//
// This is part of the DB interface.
func (s *SQLStore) Fail(paymentHash lntypes.Hash,
	failureReason FailureReason) (*MPPayment, error) {

	return nil, nil
}

// DeleteFailedAttempts removes all failed HTLCs from the db. It should
// be called for a given payment whenever all inflight htlcs are
// completed, and the payment has reached a final terminal state.
//
// This is part of the DB interface.
func (s *SQLStore) DeleteFailedAttempts(paymentHash lntypes.Hash) error {
	return nil
}
