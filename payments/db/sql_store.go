package paymentsdb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/ltcsuite/ltcd/btcec"
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

	FetchPayment(ctx context.Context, paymentHash []byte) (sqlc.Payment, error)
	FetchPayments(ctx context.Context, paymentHashes [][]byte) ([]sqlc.Payment, error)

	/*
		Payment DB write operations.
	*/
	DeletePayment(ctx context.Context, paymentHash []byte) error
	DeletePayments(ctx context.Context, paymentIDs []int64) error
	DeleteFailedAttempts(ctx context.Context, paymentID int64) error
	DeleteFailedAttemptsByAttemptIndices(ctx context.Context, attemptIndices []int64) error

	InsertPayment(ctx context.Context, payment sqlc.InsertPaymentParams) (int64, error)
	InsertFirstHopCustomRecord(ctx context.Context, customRecord sqlc.InsertFirstHopCustomRecordParams) error
	InsertHtlcAttempt(ctx context.Context, attempt sqlc.InsertHtlcAttemptParams) (int64, error)
	InsertHtlAttemptFirstHopCustomRecord(ctx context.Context, customRecord sqlc.InsertHtlAttemptFirstHopCustomRecordParams) error
	InsertHop(ctx context.Context, hop sqlc.InsertHopParams) (int64, error)
	InsertHopCustomRecord(ctx context.Context, customRecord sqlc.InsertHopCustomRecordParams) error

	UpdateHtlcAttemptSettleInfo(ctx context.Context, settleInfo sqlc.UpdateHtlcAttemptSettleInfoParams) (int64, error)
	UpdateHtlcAttemptFailInfo(ctx context.Context, failInfo sqlc.UpdateHtlcAttemptFailInfoParams) (int64, error)
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

		// If the payment is succeeded and we are not interested in
		// incomplete payments, we skip it.
		if mpPayment.Status == StatusSucceeded &&
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
	var (
		ctx        = context.Background()
		mppPayment *MPPayment
	)

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("unable to fetch payment: %w", err)
		}

		payment, err := s.fetchPaymentWithCompleteData(
			ctx, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch complete payment "+
				"data: %w", err)
		}

		mppPayment = payment

		return nil
	}, func() {
		mppPayment = nil
	})

	if err != nil {
		return nil, fmt.Errorf("unable to fetch payment: %w", err)
	}

	return mppPayment, nil
}

// FetchInFlightPayments fetches all payments which are INFLIGHT.
//
// This is part of the DB interface.
func (s *SQLStore) FetchInFlightPayments() ([]*MPPayment, error) {
	var (
		ctx              = context.Background()
		inFlightPayments []*MPPayment
	)

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbInflightAttempts, err := db.FetchHtlcAttempts(
			ctx, sqlc.FetchHtlcAttemptsParams{
				InFlightOnly: true,
			},
		)
		if err != nil {
			return fmt.Errorf("unable to fetch inflight htlc "+
				"attempts: %w", err)
		}

		// This makes sure we remove all duplicate payment hashes in
		// cases where multiple attempts for the same payment are
		// INFLIGHT.
		paymentHashes := make([][]byte, len(dbInflightAttempts))
		for i, attempt := range dbInflightAttempts {
			paymentHashes[i] = attempt.PaymentHash[:]
		}

		dbPayments, err := db.FetchPayments(ctx, paymentHashes)
		if err != nil {
			return fmt.Errorf("unable to fetch payments: %w", err)
		}

		// pre-allocate the slice to the number of payments.
		inFlightPayments = make([]*MPPayment, len(dbPayments))

		for _, dbPayment := range dbPayments {
			// NOTE: There is a small inefficency here as we fetch
			// the payment attempts for each payment again, this
			// could be improved by reusing the data from the
			// previous fetch.
			mppPayment, err := s.fetchPaymentWithCompleteData(
				ctx, db, dbPayment,
			)
			if err != nil {
				return fmt.Errorf("unable to fetch payment: %w",
					err)
			}

			inFlightPayments = append(inFlightPayments, mppPayment)
		}

		return nil
	}, func() {
		inFlightPayments = nil
	})

	if err != nil {
		return nil, fmt.Errorf("unable to fetch in-flight "+
			"payments: %w", err)
	}

	return inFlightPayments, nil
}

// DeletePayment deletes a payment from the database given its payment hash.
// It will only delete the failed attempts if the failedAttemptsOnly flag is
// set.
//
// This is part of the DB interface.
func (s *SQLStore) DeletePayment(paymentHash lntypes.Hash,
	failedAttemptsOnly bool) error {

	ctx := context.Background()

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		payment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("payment not found: %w",
					ErrPaymentNotInitiated)
			}
			return fmt.Errorf("unable to fetch payment: %w", err)
		}

		// We need to fetch the complete payment data to check if the
		// payment is removable.
		completePayment, err := s.fetchPaymentWithCompleteData(
			ctx, db, payment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch complete "+
				"payment data: %w", err)
		}
		if err := completePayment.Status.removable(); err != nil {
			return fmt.Errorf("payment is still in flight: %w", err)
		}

		// If we selected to only delete failed attempts, we only
		// delete the failed attempts rather than the payment itself.
		if failedAttemptsOnly {
			err = db.DeleteFailedAttempts(ctx, payment.ID)
			if err != nil {
				return fmt.Errorf("unable to delete failed "+
					"attempts: %w", err)
			}
		} else {
			err = db.DeletePayment(ctx, paymentHash[:])
			if err != nil {
				return fmt.Errorf("unable to delete "+
					"payment: %w", err)
			}
		}

		return nil
	}, func() {})

	if err != nil {
		return fmt.Errorf("unable to delete payment: %w", err)
	}

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

	var (
		ctx            = context.Background()
		totalDeleted   int
		paymentIDs     = make([]int64, 0)
		attemptIndices = make([]int64, 0)
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

		// We skip payments which are still INFLIGHT.
		if err := mpPayment.Status.removable(); err != nil {
			return nil
		}

		// We skip payments which are settled.
		if failedOnly && mpPayment.Status != StatusFailed {
			return nil
		}

		// If we are only interested in failed attempts, we only add
		// the attempt indices to the slice.
		if failedAttemptsOnly {
			for _, attempt := range mpPayment.HTLCs {
				if attempt.Failure != nil {
					attemptIndices = append(
						attemptIndices,
						int64(attempt.AttemptID),
					)
				}
			}

			return nil
		}

		// Otherwise we add the whole payment to the slice.
		paymentIDs = append(paymentIDs, int64(mpPayment.SequenceNum))

		return nil
	}

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		queryFunc := func(ctx context.Context, lastID int64,
			limit int32) ([]sqlc.Payment, error) {

			filterParams := sqlc.FilterPaymentsParams{
				NumLimit: limit,
				IndexOffsetGet: sqldb.SQLInt64(
					lastID,
				),
			}

			return db.FilterPayments(ctx, filterParams)
		}

		// We start at the first payment.
		initialCursor := int64(-1)

		err := sqldb.ExecutePaginatedQuery(
			ctx, s.cfg.QueryCfg, initialCursor, queryFunc,
			extractCursor, processPayment,
		)
		if err != nil {
			return fmt.Errorf("unable to paginate payments: %w",
				err)
		}

		// Now that we have all the payments or attempt indices, we
		// can delete them in batches.
		if len(paymentIDs) > 0 {
			// First we set the deleted count to the number of
			// payments we are going to delete.
			totalDeleted = len(paymentIDs)

			return sqldb.ExecuteBatchQuery(
				ctx, s.cfg.QueryCfg, paymentIDs,
				func(id int64) int64 {
					return id
				},
				func(ctx context.Context, ids []int64) ([]any, error) {
					return nil, db.DeletePayments(ctx, ids)
				},
				nil,
			)
		}

		//nolint:ll
		if len(attemptIndices) > 0 {
			return sqldb.ExecuteBatchQuery(
				ctx, s.cfg.QueryCfg, attemptIndices,
				func(id int64) int64 {
					return id
				},
				func(ctx context.Context, ids []int64) ([]any, error) {
					return nil, db.DeleteFailedAttemptsByAttemptIndices(
						ctx, ids,
					)
				},
				nil,
			)
		}

		return nil
	}, func() {
		paymentIDs = nil
		attemptIndices = nil
		totalDeleted = 0
	})

	if err != nil {
		return 0, fmt.Errorf("unable to delete payments: %w", err)
	}

	return totalDeleted, nil
}

// InitPayment checks that no other payment with the same payment hash
// exists in the database before creating a new payment. When this
// method returns successfully, the payment is guaranteed to be in the
// InFlight state.
//
// This is part of the DB interface.
func (s *SQLStore) InitPayment(paymentHash lntypes.Hash,
	creationInfo *PaymentCreationInfo) error {

	ctx := context.Background()

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// First, try to fetch the existing payment to check
		// its status.
		dbPayment, err := db.FetchPayment(
			ctx, paymentHash[:],
		)
		if err != nil {
			// If the payment doesn't exist, we can proceed
			// with initialization.
			if errors.Is(err, sql.ErrNoRows) {
				// Payment doesn't exist, we can
				// initialize it.
				return s.insertNewPayment(
					ctx, db, paymentHash,
					creationInfo,
				)
			}

			// Some other error occurred, return it.
			return fmt.Errorf("unable to fetch "+
				"existing payment: %w", err)
		}

		// We fetch the complete payment data to determine if
		// a new payment can be initialized.
		payment, err := s.fetchPaymentWithCompleteData(
			ctx, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch "+
				"complete payment data: %w", err)
		}

		// If the payment is not initializable, we return an
		// error.
		// This is only the case if the payment is already in
		// the failed state.
		if err := payment.Status.initializable(); err != nil {
			return fmt.Errorf("payment is not "+
				"initializable: %w", err)
		}

		// This should never happen, but we check it for
		// completeness.
		if err := payment.Status.removable(); err != nil {
			return fmt.Errorf("payment is not "+
				"removable: %w", err)
		}

		// We delete the payment to avoid duplicate payments, moreover
		// there is a unique constraint on the payment hash so we would
		// not be able to insert a new payment with the same hash.
		err = db.DeletePayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("unable to delete "+
				"payment: %w", err)
		}

		// And insert a new payment.
		err = s.insertNewPayment(ctx, db, paymentHash, creationInfo)
		if err != nil {
			return fmt.Errorf("unable to insert "+
				"new payment: %w", err)
		}

		return nil
	}, func() {})
	if err != nil {
		return fmt.Errorf("unable to init payment: %w", err)
	}

	return nil
}

// insertNewPayment inserts a new payment into the database.
func (s *SQLStore) insertNewPayment(ctx context.Context, db SQLQueries,
	paymentHash lntypes.Hash, creationInfo *PaymentCreationInfo) error {

	// Create the payment insert parameters.
	insertParams := sqlc.InsertPaymentParams{
		PaymentRequest: creationInfo.PaymentRequest,
		AmountMsat:     int64(creationInfo.Value),
		CreatedAt:      creationInfo.CreationTime.UTC(),
		PaymentHash:    paymentHash[:],
	}

	// Insert the payment and get the payment ID.
	paymentID, err := db.InsertPayment(ctx, insertParams)
	if err != nil {
		return fmt.Errorf("unable to insert payment: %w", err)
	}

	// If there are first hop custom records, we insert them now.
	if len(creationInfo.FirstHopCustomRecords) > 0 {
		err := creationInfo.FirstHopCustomRecords.Validate()
		if err != nil {
			return fmt.Errorf("invalid first hop custom "+
				"records: %w", err)
		}

		for key, value := range creationInfo.FirstHopCustomRecords {
			err = db.InsertFirstHopCustomRecord(
				ctx, sqlc.InsertFirstHopCustomRecordParams{
					PaymentID: paymentID,
					Key:       int64(key),
					Value:     value,
				},
			)
			if err != nil {
				return fmt.Errorf("unable to insert first "+
					"hop custom records: %w", err)
			}
		}
	}

	return nil
}

// RegisterAttempt atomically records the provided HTLCAttemptInfo.
//
// This is part of the DB interface.
func (s *SQLStore) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *HTLCAttemptInfo) (*MPPayment, error) {

	var (
		ctx        = context.Background()
		mppPayment *MPPayment
	)

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		dbPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("unable to fetch payment: %w", err)
		}

		// We need to fetch the complete payment to make sure we can
		// register a new attempt.
		payment, err := s.fetchPaymentWithCompleteData(
			ctx, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch "+
				"complete payment data: %w", err)
		}

		// Check if registering a new attempt is allowed.
		if err := payment.Registrable(); err != nil {
			return err
		}

		// Verify the attempt is compatible with the existing payment.
		if err := verifyAttempt(payment, attempt); err != nil {
			return err
		}

		// The attempt is valid, we can insert it now.
		err = s.insertHtlcAttemptWithHops(ctx, db, payment, attempt)
		if err != nil {
			return fmt.Errorf("unable to insert htlc attempt: %w",
				err)
		}

		// Add the new attempts to the payment so we don't have to fetch
		// the payment again because we have all the data we need.
		payment.HTLCs = append(payment.HTLCs, HTLCAttempt{
			HTLCAttemptInfo: *attempt,
		})

		// We update the payment because the state is now different and
		// we want to return the updated payment.
		err = payment.setState()
		if err != nil {
			return fmt.Errorf("unable to set state: %w", err)
		}

		mppPayment = payment

		return nil
	}, func() {
		mppPayment = nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to register attempt: %w", err)
	}

	return mppPayment, nil
}

// insertHtlcAttemptWithHops inserts a new HTLC attempt and all its associated
// hop data into the database. This function handles the complete insertion
// of an HTLC attempt including:
//   - The HTLC attempt record itself
//   - All route hops for the attempt
//   - Custom records for each hop
func (s *SQLStore) insertHtlcAttemptWithHops(ctx context.Context, db SQLQueries,
	mpPayment *MPPayment, attempt *HTLCAttemptInfo) error {

	// Insert the main HTLC attempt record.
	err := s.insertHtlcAttempt(ctx, db, mpPayment, attempt)
	if err != nil {
		return fmt.Errorf("unable to insert htlc attempt record: %w",
			err)
	}

	// Insert all hops for this attempt
	if len(attempt.Route.Hops) > 0 {
		err := s.insertHtlcAttemptHops(ctx, db, attempt)
		if err != nil {
			return fmt.Errorf("unable to insert htlc attempt "+
				"hops: %w", err)
		}
	}

	return nil
}

// insertHtlcAttempt inserts the main HTLC attempt record into the database.
func (s *SQLStore) insertHtlcAttempt(ctx context.Context, db SQLQueries,
	mpPayment *MPPayment, attempt *HTLCAttemptInfo) error {

	var sessionKey [btcec.PrivKeyBytesLen]byte
	copy(sessionKey[:], attempt.SessionKey().Serialize())

	var routeSourceKey [btcec.PubKeyBytesLenCompressed]byte
	copy(routeSourceKey[:], attempt.Route.SourcePubKey[:])

	// Insert the HTLC attempt using named parameters
	_, err := db.InsertHtlcAttempt(ctx, sqlc.InsertHtlcAttemptParams{
		AttemptIndex:       int64(attempt.AttemptID),
		PaymentID:          int64(mpPayment.SequenceNum),
		PaymentHash:        mpPayment.Info.PaymentIdentifier[:],
		AttemptTime:        attempt.AttemptTime.UTC(),
		SessionKey:         sessionKey[:],
		RouteTotalTimeLock: int32(attempt.Route.TotalTimeLock),
		RouteTotalAmount:   int64(attempt.Route.TotalAmount),
		RouteSourceKey:     routeSourceKey[:],
		FirstHopAmountMsat: int64(
			attempt.Route.FirstHopAmount.Val.Int(),
		),
	})
	if err != nil {
		return fmt.Errorf("unable to insert htlc attempt: %w", err)
	}

	// Insert the custom records for the route.
	for key, value := range attempt.Route.FirstHopWireCustomRecords {
		err = db.InsertHtlAttemptFirstHopCustomRecord(
			ctx, sqlc.InsertHtlAttemptFirstHopCustomRecordParams{
				HtlcAttemptIndex: int64(attempt.AttemptID),
				Key:              int64(key),
				Value:            value,
			},
		)
		if err != nil {
			return fmt.Errorf("unable to insert htlc attempt "+
				"first hop custom record: %w", err)
		}
	}

	return nil
}

// insertHtlcAttemptHops inserts all hops for a given HTLC attempt.
func (s *SQLStore) insertHtlcAttemptHops(ctx context.Context, db SQLQueries,
	attempt *HTLCAttemptInfo) error {

	for index, hop := range attempt.Route.Hops {
		// Insert the hop record
		hopID, err := s.insertHop(
			ctx, db, attempt.AttemptID, index, hop,
		)
		if err != nil {
			return fmt.Errorf("unable to insert hop record: %w",
				err)
		}

		// Insert custom records for this hop if any exist
		if len(hop.CustomRecords) > 0 {
			err := hop.CustomRecords.Validate()
			if err != nil {
				return fmt.Errorf("invalid hop custom "+
					"records: %w", err)
			}

			for key, value := range hop.CustomRecords {
				err = db.InsertHopCustomRecord(
					ctx, sqlc.InsertHopCustomRecordParams{
						HopID: hopID,
						Key:   int64(key),
						Value: value,
					},
				)
				if err != nil {
					return fmt.Errorf("unable to insert "+
						"hop custom records: %w", err)
				}
			}
		}
	}

	return nil
}

// insertHop inserts a single hop record into the database.
func (s *SQLStore) insertHop(ctx context.Context, db SQLQueries,
	attemptID uint64, hopIndex int, hop *route.Hop) (int64, error) {

	// Insert the hop using named parameters
	hopID, err := db.InsertHop(ctx, sqlc.InsertHopParams{
		HtlcAttemptIndex: int64(attemptID),
		HopIndex:         int32(hopIndex),
		PubKey:           hop.PubKeyBytes[:],
		Scid:             strconv.FormatUint(hop.ChannelID, 10),
		OutgoingTimeLock: int32(hop.OutgoingTimeLock),
		AmtToForward:     int64(hop.AmtToForward),
		MetaData:         hop.Metadata,
		LegacyPayload:    hop.LegacyPayload,
		// Handle MPP (Multi-Path Payment) data if present.
		MppPaymentAddr: s.extractMppPaymentAddr(hop),
		MppTotalMsat:   s.extractMppTotalMsat(hop),
		// Handle AMP (Atomic Multi-Path) data if present.
		AmpRootShare:  s.extractAmpRootShare(hop),
		AmpSetID:      s.extractAmpSetID(hop),
		AmpChildIndex: s.extractAmpChildIndex(hop),
		// Handle blinding point data if present.
		BlindingPoint: s.extractBlindingPoint(hop),
		// Handle encrypted data for blinded paths if present.
		EncryptedData: hop.EncryptedData,
		// Handle blinded path total amount if present.
		BlindedPathTotalAmt: s.extractBlindedPathTotalAmt(hop),
	})
	if err != nil {
		return 0, fmt.Errorf("unable to insert hop: %w", err)
	}

	return hopID, nil
}

// extractMppPaymentAddr extracts the MPP payment address from a hop.
func (s *SQLStore) extractMppPaymentAddr(hop *route.Hop) []byte {
	if hop.MPP != nil {
		paymentAddr := hop.MPP.PaymentAddr()
		return paymentAddr[:]
	}

	return nil
}

// extractMppTotalMsat extracts the MPP total amount from a hop.
func (s *SQLStore) extractMppTotalMsat(hop *route.Hop) sql.NullInt64 {
	if hop.MPP != nil {
		return sql.NullInt64{
			Int64: int64(hop.MPP.TotalMsat()),
			Valid: true,
		}
	}

	return sql.NullInt64{Valid: false}
}

// extractAmpRootShare extracts the AMP root share from a hop.
func (s *SQLStore) extractAmpRootShare(hop *route.Hop) []byte {
	if hop.AMP != nil {
		rootShare := hop.AMP.RootShare()
		return rootShare[:]
	}

	return nil
}

// extractAmpSetID extracts the AMP set ID from a hop.
func (s *SQLStore) extractAmpSetID(hop *route.Hop) []byte {
	if hop.AMP != nil {
		ampSetID := hop.AMP.SetID()
		return ampSetID[:]
	}

	return nil
}

// extractAmpChildIndex extracts the AMP child index from a hop.
func (s *SQLStore) extractAmpChildIndex(hop *route.Hop) sql.NullInt32 {
	if hop.AMP != nil {
		return sql.NullInt32{
			Int32: int32(hop.AMP.ChildIndex()),
			Valid: true,
		}
	}

	return sql.NullInt32{Valid: false}
}

// extractBlindingPoint extracts the blinding point from a hop.
func (s *SQLStore) extractBlindingPoint(hop *route.Hop) []byte {
	if hop.BlindingPoint != nil {
		blindingPoint := hop.BlindingPoint.SerializeCompressed()
		return blindingPoint[:]
	}

	return nil
}

// extractBlindedPathTotalAmt extracts the blinded path total amount from a hop.
func (s *SQLStore) extractBlindedPathTotalAmt(hop *route.Hop) sql.NullInt64 {
	if hop.EncryptedData != nil {
		return sql.NullInt64{
			Int64: int64(hop.TotalAmtMsat),
			Valid: true,
		}
	}

	return sql.NullInt64{Valid: false}
}

// SettleAttempt marks the given attempt settled with the preimage. If
// this is a multi shard payment, this might implicitly mean the
// full payment succeeded.
//
// This is part of the DB interface.
func (s *SQLStore) SettleAttempt(paymentHash lntypes.Hash,
	attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error) {

	var (
		ctx        = context.Background()
		mppPayment *MPPayment
	)

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		dbPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPaymentNotInitiated
			}

			return fmt.Errorf("unable to fetch payment: %w", err)
		}

		payment, err := s.fetchPaymentWithCompleteData(
			ctx, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch complete payment "+
				"data: %w", err)
		}

		if err := payment.Status.updatable(); err != nil {
			return fmt.Errorf("payment is not updatable: %w", err)
		}

		// Update the HTLC attempt with settlement information.
		_, err = db.UpdateHtlcAttemptSettleInfo(ctx,
			sqlc.UpdateHtlcAttemptSettleInfoParams{
				AttemptIndex:   int64(attemptID),
				SettlePreimage: settleInfo.Preimage[:],
				SettleTime: sql.NullTime{
					Time:  settleInfo.SettleTime.UTC(),
					Valid: true,
				},
			})
		if err != nil {
			return fmt.Errorf("unable to update htlc attempt "+
				"settle info: %w", err)
		}

		// After updating the HTLC attempt, we need to fetch the
		// updated payment again.
		//
		// NOTE: No need to fetch the main payment data again because
		// nothing changed there.
		payment, err = s.fetchPaymentWithCompleteData(
			ctx, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch complete payment "+
				"data: %w", err)
		}

		mppPayment = payment

		return nil
	}, func() {
		mppPayment = nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to settle attempt: %w", err)
	}

	return mppPayment, nil
}

// FailAttempt marks the given payment attempt failed.
//
// This is part of the DB interface.
func (s *SQLStore) FailAttempt(paymentHash lntypes.Hash,
	attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error) {

	var (
		ctx        = context.Background()
		mppPayment *MPPayment
	)

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		dbPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("unable to fetch payment: %w", err)
		}

		payment, err := s.fetchPaymentWithCompleteData(
			ctx, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch complete payment "+
				"data: %w", err)
		}

		if err := payment.Status.updatable(); err != nil {
			return fmt.Errorf("payment is not updatable: %w", err)
		}

		updateParams := sqlc.UpdateHtlcAttemptFailInfoParams{
			AttemptIndex: int64(attemptID),
			FailureSourceIndex: sql.NullInt32{
				Int32: int32(failInfo.FailureSourceIndex),
				Valid: true,
			},
			HtlcFailReason: sql.NullInt32{
				Int32: int32(failInfo.Reason),
				Valid: true,
			},
			FailTime: sql.NullTime{
				Time:  failInfo.FailTime.UTC(),
				Valid: true,
			},
		}

		// If the failure message is not nil, we need to encode it.
		if failInfo.Message != nil {
			buf := bytes.NewBuffer(nil)
			lnwire.EncodeFailureMessage(buf, failInfo.Message, 0)
			updateParams.FailureMsg = buf.Bytes()
		}

		// Update the HTLC attempt with failure information
		_, err = db.UpdateHtlcAttemptFailInfo(ctx, updateParams)
		if err != nil {
			return fmt.Errorf("unable to update htlc attempt"+
				"fail info: %w", err)
		}

		// After updating the HTLC attempt, we need to fetch the
		// updated payment again.
		//
		// NOTE: No need to fetch the main payment data again because
		// nothing changed there. The failure reason is updated in
		// `Fail` function only.
		payment, err = s.fetchPaymentWithCompleteData(
			ctx, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch complete payment "+
				"data: %w", err)
		}

		mppPayment = payment

		return nil
	}, func() {
		mppPayment = nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to fail attempt: %w", err)
	}

	return mppPayment, nil
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
