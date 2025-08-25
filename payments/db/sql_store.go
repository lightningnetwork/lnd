package paymentsdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// SQLQueries is a subset of the sqlc.Querier interface that can be used to
// execute queries against the SQL payments tables.
//
//nolint:ll,interfacebloat
type SQLQueries interface {
	/*
		Payment DB read operations.
	*/
	FilterPayments(ctx context.Context, query sqlc.FilterPaymentsParams) ([]sqlc.Payment, error)
	CountPayments(ctx context.Context) (int64, error)
	FetchHtlcAttempts(ctx context.Context, query sqlc.FetchHtlcAttemptsParams) ([]sqlc.PaymentHtlcAttempt, error)
	FetchCustomRecordsForAttempts(ctx context.Context, attemptIndices []int64) ([]sqlc.PaymentHtlcAttemptCustomRecord, error)
	FetchHopsForAttempts(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.PaymentRouteHop, error)
	FetchFirstHopCustomRecords(ctx context.Context, paymentID int64) ([]sqlc.PaymentFirstHopCustomRecord, error)
	FetchCustomRecordsForHops(ctx context.Context, hopIDs []int64) ([]sqlc.PaymentRouteHopCustomRecord, error)
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

	if query.MaxPayments == 0 {
		return Response{}, fmt.Errorf("max payments must be non-zero")
	}

	var (
		allPayments   []*MPPayment
		totalCount    int64
		initialCursor int64
	)

	extractCursor := func(
		row sqlc.Payment) int64 {

		return row.ID
	}

	processPayment := func(ctx context.Context,
		dbPayment sqlc.Payment) error {

		// Now we need to fetch all the additional data for the payment.
		mpPayment, err := s.fetchPaymentWithCompleteData(
			ctx, s.db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		// To keep compatibility with the old API, we only
		// return non-succeeded payments if requested.
		if mpPayment.Status != StatusSucceeded &&
			!query.IncludeIncomplete {

			return nil
		}

		if len(allPayments) >= int(query.MaxPayments) {
			return ErrMaxPaymentsReached
		}

		allPayments = append(allPayments, mpPayment)

		return nil
	}

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		// We first count all payments to determine the total count
		// if requested.
		if query.CountTotal {
			totalPayments, err := db.CountPayments(ctx)
			if err != nil {
				return fmt.Errorf("failed to count "+
					"payments: %w", err)
			}
			totalCount = totalPayments
		}

		queryFunc := func(ctx context.Context, lastID int64,
			limit int32) ([]sqlc.Payment, error) {

			filterParams := sqlc.FilterPaymentsParams{
				NumLimit: limit,
				Reverse:  query.Reversed,
			}

			if query.Reversed {
				filterParams.IndexOffsetLet = sqldb.SQLInt64(
					lastID,
				)
			} else {
				filterParams.IndexOffsetGet = sqldb.SQLInt64(
					lastID,
				)
			}

			// Add potential date filters if specified.
			if query.CreationDateStart != 0 {
				filterParams.CreatedAfter = sqldb.SQLTime(
					time.Unix(query.CreationDateStart, 0).
						UTC(),
				)
			}
			if query.CreationDateEnd != 0 {
				filterParams.CreatedBefore = sqldb.SQLTime(
					time.Unix(query.CreationDateEnd, 0).
						UTC(),
				)
			}

			// If we are only interested in settled payments (no
			// failed or in-flight payments), we can exclude failed
			// the failed one here with the query params. We will
			// still fetch INFLIGHT payments which we then will
			// drop in the `handlePayment` function. There is no
			// easy way currently to also not fetch INFLIGHT
			// payments.
			//
			// NOTE: This is set in place to keep compatibility with
			// the KV implementation.
			if !query.IncludeIncomplete {
				filterParams.ExcludeFailed = sqldb.SQLBool(true)
			}

			return db.FilterPayments(ctx, filterParams)
		}

		if query.Reversed {
			initialCursor = int64(math.MaxInt64)
		} else {
			initialCursor = int64(-1)
		}

		return sqldb.ExecutePaginatedQuery(
			ctx, s.cfg.QueryCfg, initialCursor, queryFunc,
			extractCursor, processPayment,
		)

	}, func() {
		allPayments = nil
	})

	// If make sure we don't return an error if we reached the maximum
	// number of payments. Which is the pagination limit for the query
	// itself.
	if err != nil && !errors.Is(err, ErrMaxPaymentsReached) {
		return Response{}, fmt.Errorf("failed to query payments: %w",
			err)
	}

	// Handle case where no payments were found
	if len(allPayments) == 0 {
		return Response{
			Payments:         allPayments,
			FirstIndexOffset: 0,
			LastIndexOffset:  0,
			TotalCount:       uint64(totalCount),
		}, nil
	}

	return Response{
		Payments:         allPayments,
		FirstIndexOffset: uint64(allPayments[0].SequenceNum),
		LastIndexOffset: uint64(allPayments[len(allPayments)-1].
			SequenceNum),
		TotalCount: uint64(totalCount),
	}, nil
}

// fetchPaymentWithCompleteData fetches a payment and all its associated data
// (HTLC attempts, hops, custom records) to create a complete internal
// representation of the payment.
//
// NOTE: This does also add the payment status and payment state which is
// derived from the db data.
func (s *SQLStore) fetchPaymentWithCompleteData(ctx context.Context,
	db SQLQueries, dbPayment sqlc.Payment) (*MPPayment, error) {

	// We fetch all the htlc attempts for the payment.
	dbHtlcAttempts, err := db.FetchHtlcAttempts(
		ctx, sqlc.FetchHtlcAttemptsParams{
			PaymentID: dbPayment.ID,
		},
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unable to fetch htlc attempts for "+
			"payment(id=%d): %w", dbPayment.ID, err)
	}

	// Sort attempts by ID to ensure correct order so that the attempts are
	// processed in the correct order.
	sort.Slice(dbHtlcAttempts, func(i, j int) bool {
		return dbHtlcAttempts[i].ID < dbHtlcAttempts[j].ID
	})

	if len(dbHtlcAttempts) == 0 {
		payment, err := unmarshalPaymentWithoutHTLCs(dbPayment)
		if err != nil {
			return nil, fmt.Errorf("unable to create payment "+
				"without HTLCs: %w", err)
		}

		// We get the state of the payment in case it has inflight
		// HTLCs.
		//
		// TODO(ziggie): Refactor this code so we can determine the
		// payment state without having to fetch all HTLCs.
		err = payment.setState()
		if err != nil {
			return nil, fmt.Errorf("unable to set state: %w", err)
		}

		return payment, nil
	}

	// Get all hops for all attempts.
	attemptIndices := extractAttemptIndices(dbHtlcAttempts)
	dbHops, err := db.FetchHopsForAttempts(ctx, attemptIndices)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unable to fetch hops: %w", err)
	}

	// Get all custom records for all attempts.
	dbRouteCustomRecords, err := db.FetchCustomRecordsForAttempts(
		ctx, attemptIndices,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unable to fetch route custom "+
			"records: %w", err)
	}

	// Get all custom records for all hops.
	hopIDs := extractHopIDs(dbHops)
	dbHopCustomRecords, err := db.FetchCustomRecordsForHops(ctx, hopIDs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unable to fetch custom records: %w",
			err)
	}

	dbFirstHopCustomRecords, err := db.FetchFirstHopCustomRecords(
		ctx, dbPayment.ID,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unable to fetch first hop "+
			"custom records for payment(id=%d): %w",
			dbPayment.ID, err)
	}

	payment, err := unmarshalPaymentData(
		dbPayment, dbHtlcAttempts, dbHops, dbRouteCustomRecords,
		dbHopCustomRecords, dbFirstHopCustomRecords,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to process payment data: %w",
			err)
	}

	// We get the state of the payment in case it has inflight HTLCs.
	//
	// TODO(ziggie): Refactor this code so we can determine the payment
	// state without having to fetch all HTLCs.
	err = payment.setState()
	if err != nil {
		return nil, fmt.Errorf("unable to set state: %w", err)
	}

	return payment, nil
}

// extractAttemptIndices extracts the attempt indices from a slice of
// sqlc.PaymentHtlcAttempt.
func extractAttemptIndices(attempts []sqlc.PaymentHtlcAttempt) []int64 {
	indices := make([]int64, len(attempts))
	for i, attempt := range attempts {
		indices[i] = attempt.AttemptIndex
	}

	return indices
}

// extractHopIDs extracts the hop IDs from a slice of sqlc.PaymentRouteHop.
func extractHopIDs(hops []sqlc.PaymentRouteHop) []int64 {
	ids := make([]int64, len(hops))
	for i, hop := range hops {
		ids[i] = hop.ID
	}

	return ids
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
