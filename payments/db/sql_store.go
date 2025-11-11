package paymentsdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// PaymentIntentType represents the type of payment intent.
type PaymentIntentType int16

const (
	// PaymentIntentTypeBolt11 indicates a BOLT11 invoice payment.
	PaymentIntentTypeBolt11 PaymentIntentType = 0
)

// HTLCAttemptResolutionType represents the type of HTLC attempt resolution.
type HTLCAttemptResolutionType int32

const (
	// HTLCAttemptResolutionSettled indicates the HTLC attempt was settled
	// successfully with a preimage.
	HTLCAttemptResolutionSettled HTLCAttemptResolutionType = 1

	// HTLCAttemptResolutionFailed indicates the HTLC attempt failed.
	HTLCAttemptResolutionFailed HTLCAttemptResolutionType = 2
)

// SQLQueries is a subset of the sqlc.Querier interface that can be used to
// execute queries against the SQL payments tables.
//
//nolint:ll
type SQLQueries interface {
	/*
		Payment DB read operations.
	*/
	FilterPayments(ctx context.Context, query sqlc.FilterPaymentsParams) ([]sqlc.FilterPaymentsRow, error)
	FetchPayment(ctx context.Context, paymentIdentifier []byte) (sqlc.FetchPaymentRow, error)
	FetchPaymentsByIDs(ctx context.Context, paymentIDs []int64) ([]sqlc.FetchPaymentsByIDsRow, error)

	CountPayments(ctx context.Context) (int64, error)

	FetchHtlcAttemptsForPayments(ctx context.Context, paymentIDs []int64) ([]sqlc.FetchHtlcAttemptsForPaymentsRow, error)
	FetchAllInflightAttempts(ctx context.Context) ([]sqlc.PaymentHtlcAttempt, error)
	FetchHopsForAttempts(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.FetchHopsForAttemptsRow, error)

	FetchPaymentLevelFirstHopCustomRecords(ctx context.Context, paymentIDs []int64) ([]sqlc.PaymentFirstHopCustomRecord, error)
	FetchRouteLevelFirstHopCustomRecords(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.PaymentAttemptFirstHopCustomRecord, error)
	FetchHopLevelCustomRecords(ctx context.Context, hopIDs []int64) ([]sqlc.PaymentHopCustomRecord, error)
}

// BatchedSQLQueries is a version of the SQLQueries that's capable
// of batched database operations.
type BatchedSQLQueries interface {
	SQLQueries
	sqldb.BatchedTx[SQLQueries]
}

// SQLStore represents a storage backend.
type SQLStore struct {
	// TODO(ziggie): Remove the KVStore once all the interface functions are
	// implemented.
	KVStore

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

// NewSQLStore creates a new SQLStore instance given an open
// BatchedSQLPaymentsQueries storage backend.
func NewSQLStore(cfg *SQLStoreConfig, db BatchedSQLQueries,
	options ...OptionModifier) (*SQLStore, error) {

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

// A compile-time constraint to ensure SQLStore implements DB.
var _ DB = (*SQLStore)(nil)

// fetchPaymentWithCompleteData fetches a payment with all its related data
// including attempts, hops, and custom records from the database.
// This is a convenience wrapper around the batch loading functions for single
// payment operations.
func (s *SQLStore) fetchPaymentWithCompleteData(ctx context.Context,
	db SQLQueries, dbPayment sqlc.PaymentAndIntent) (*MPPayment, error) {

	payment := dbPayment.GetPayment()

	// Load batch data for this single payment.
	batchData, err := s.loadPaymentsBatchData(ctx, db, []int64{payment.ID})
	if err != nil {
		return nil, fmt.Errorf("failed to load batch data: %w", err)
	}

	// Build the payment from the batch data.
	return s.buildPaymentFromBatchData(dbPayment, batchData)
}

// paymentsBatchData holds all the batch-loaded data for multiple payments.
type paymentsBatchData struct {
	// paymentCustomRecords maps payment ID to its custom records.
	paymentCustomRecords map[int64][]sqlc.PaymentFirstHopCustomRecord

	// attempts maps payment ID to its HTLC attempts.
	attempts map[int64][]sqlc.FetchHtlcAttemptsForPaymentsRow

	// hopsByAttempt maps attempt index to its hops.
	hopsByAttempt map[int64][]sqlc.FetchHopsForAttemptsRow

	// hopCustomRecords maps hop ID to its custom records.
	hopCustomRecords map[int64][]sqlc.PaymentHopCustomRecord

	// routeCustomRecords maps attempt index to its route-level custom
	// records.
	routeCustomRecords map[int64][]sqlc.PaymentAttemptFirstHopCustomRecord
}

// loadPaymentCustomRecords loads payment-level custom records for a given
// set of payment IDs. It uses a batch query to fetch all custom records for
// the given payment IDs.
func (s *SQLStore) loadPaymentCustomRecords(ctx context.Context,
	db SQLQueries, paymentIDs []int64,
	batchData *paymentsBatchData) error {

	return sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, paymentIDs,
		func(id int64) int64 { return id },
		func(ctx context.Context, ids []int64) (
			[]sqlc.PaymentFirstHopCustomRecord, error) {

			//nolint:ll
			records, err := db.FetchPaymentLevelFirstHopCustomRecords(
				ctx, ids,
			)

			return records, err
		},
		func(ctx context.Context,
			record sqlc.PaymentFirstHopCustomRecord) error {

			paymentRecords :=
				batchData.paymentCustomRecords[record.PaymentID]

			batchData.paymentCustomRecords[record.PaymentID] =
				append(paymentRecords, record)

			return nil
		},
	)
}

// loadHtlcAttempts loads HTLC attempts for all payments and returns all
// attempt indices. It uses a batch query to fetch all attempts for the given
// payment IDs.
func (s *SQLStore) loadHtlcAttempts(ctx context.Context, db SQLQueries,
	paymentIDs []int64, batchData *paymentsBatchData) ([]int64, error) {

	var allAttemptIndices []int64

	err := sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, paymentIDs,
		func(id int64) int64 { return id },
		func(ctx context.Context, ids []int64) (
			[]sqlc.FetchHtlcAttemptsForPaymentsRow, error) {

			return db.FetchHtlcAttemptsForPayments(ctx, ids)
		},
		func(ctx context.Context,
			attempt sqlc.FetchHtlcAttemptsForPaymentsRow) error {

			batchData.attempts[attempt.PaymentID] = append(
				batchData.attempts[attempt.PaymentID], attempt,
			)
			allAttemptIndices = append(
				allAttemptIndices, attempt.AttemptIndex,
			)

			return nil
		},
	)

	return allAttemptIndices, err
}

// loadHopsForAttempts loads hops for all attempts and returns all hop IDs.
// It uses a batch query to fetch all hops for the given attempt indices.
func (s *SQLStore) loadHopsForAttempts(ctx context.Context, db SQLQueries,
	attemptIndices []int64, batchData *paymentsBatchData) ([]int64, error) {

	var hopIDs []int64

	err := sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, attemptIndices,
		func(idx int64) int64 { return idx },
		func(ctx context.Context, indices []int64) (
			[]sqlc.FetchHopsForAttemptsRow, error) {

			return db.FetchHopsForAttempts(ctx, indices)
		},
		func(ctx context.Context,
			hop sqlc.FetchHopsForAttemptsRow) error {

			attemptHops :=
				batchData.hopsByAttempt[hop.HtlcAttemptIndex]

			batchData.hopsByAttempt[hop.HtlcAttemptIndex] =
				append(attemptHops, hop)

			hopIDs = append(hopIDs, hop.ID)

			return nil
		},
	)

	return hopIDs, err
}

// loadHopCustomRecords loads hop-level custom records for all hops. It uses
// a batch query to fetch all custom records for the given hop IDs.
func (s *SQLStore) loadHopCustomRecords(ctx context.Context, db SQLQueries,
	hopIDs []int64, batchData *paymentsBatchData) error {

	return sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, hopIDs,
		func(id int64) int64 { return id },
		func(ctx context.Context, ids []int64) (
			[]sqlc.PaymentHopCustomRecord, error) {

			return db.FetchHopLevelCustomRecords(ctx, ids)
		},
		func(ctx context.Context,
			record sqlc.PaymentHopCustomRecord) error {

			// TODO(ziggie): Can we get rid of this?
			// This has to be in place otherwise the
			// comparison will not match.
			if record.Value == nil {
				record.Value = []byte{}
			}

			batchData.hopCustomRecords[record.HopID] = append(
				batchData.hopCustomRecords[record.HopID],
				record,
			)

			return nil
		},
	)
}

// loadRouteCustomRecords loads route-level first hop custom records for all
// attempts. It uses a batch query to fetch all custom records for the given
// attempt indices.
func (s *SQLStore) loadRouteCustomRecords(ctx context.Context, db SQLQueries,
	attemptIndices []int64, batchData *paymentsBatchData) error {

	return sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, attemptIndices,
		func(idx int64) int64 { return idx },
		func(ctx context.Context, indices []int64) (
			[]sqlc.PaymentAttemptFirstHopCustomRecord, error) {

			return db.FetchRouteLevelFirstHopCustomRecords(
				ctx, indices,
			)
		},
		func(ctx context.Context,
			record sqlc.PaymentAttemptFirstHopCustomRecord) error {

			idx := record.HtlcAttemptIndex
			attemptRecords := batchData.routeCustomRecords[idx]

			batchData.routeCustomRecords[idx] =
				append(attemptRecords, record)

			return nil
		},
	)
}

// loadPaymentsBatchData loads all related data for multiple payments in batch.
// It uses a batch queries to fetch all data for the given payment IDs.
func (s *SQLStore) loadPaymentsBatchData(ctx context.Context, db SQLQueries,
	paymentIDs []int64) (*paymentsBatchData, error) {

	batchData := &paymentsBatchData{
		paymentCustomRecords: make(
			map[int64][]sqlc.PaymentFirstHopCustomRecord,
		),
		attempts: make(
			map[int64][]sqlc.FetchHtlcAttemptsForPaymentsRow,
		),
		hopsByAttempt: make(
			map[int64][]sqlc.FetchHopsForAttemptsRow,
		),
		hopCustomRecords: make(
			map[int64][]sqlc.PaymentHopCustomRecord,
		),
		routeCustomRecords: make(
			map[int64][]sqlc.PaymentAttemptFirstHopCustomRecord,
		),
	}

	if len(paymentIDs) == 0 {
		return batchData, nil
	}

	// Load payment-level custom records.
	err := s.loadPaymentCustomRecords(ctx, db, paymentIDs, batchData)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch payment custom "+
			"records: %w", err)
	}

	// Load HTLC attempts and collect attempt indices.
	allAttemptIndices, err := s.loadHtlcAttempts(
		ctx, db, paymentIDs, batchData,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch HTLC attempts: %w",
			err)
	}

	if len(allAttemptIndices) == 0 {
		// No attempts, return early.
		return batchData, nil
	}

	// Load hops for all attempts and collect hop IDs.
	hopIDs, err := s.loadHopsForAttempts(
		ctx, db, allAttemptIndices, batchData,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch hops for attempts: %w",
			err)
	}

	// Load hop-level custom records if there are any hops.
	if len(hopIDs) > 0 {
		err = s.loadHopCustomRecords(ctx, db, hopIDs, batchData)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch hop custom "+
				"records: %w", err)
		}
	}

	// Load route-level first hop custom records.
	err = s.loadRouteCustomRecords(ctx, db, allAttemptIndices, batchData)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch route custom "+
			"records: %w", err)
	}

	return batchData, nil
}

// buildPaymentFromBatchData builds a complete MPPayment from a database payment
// and pre-loaded batch data.
func (s *SQLStore) buildPaymentFromBatchData(dbPayment sqlc.PaymentAndIntent,
	batchData *paymentsBatchData) (*MPPayment, error) {

	// The query will only return BOLT 11 payment intents or intents with
	// no intent type set.
	paymentIntent := dbPayment.GetPaymentIntent()
	paymentRequest := paymentIntent.IntentPayload

	payment := dbPayment.GetPayment()

	// Get payment-level custom records from batch data.
	customRecords := batchData.paymentCustomRecords[payment.ID]

	// Convert to the FirstHopCustomRecords map.
	var firstHopCustomRecords lnwire.CustomRecords
	if len(customRecords) > 0 {
		firstHopCustomRecords = make(lnwire.CustomRecords)
		for _, record := range customRecords {
			firstHopCustomRecords[uint64(record.Key)] = record.Value
		}
	}

	// Convert database payment data to the PaymentCreationInfo struct.
	info := dbPaymentToCreationInfo(
		payment.PaymentIdentifier, payment.AmountMsat,
		payment.CreatedAt, paymentRequest, firstHopCustomRecords,
	)

	// Get all HTLC attempts from batch data for a given payment.
	dbAttempts := batchData.attempts[payment.ID]

	// Convert all attempts to HTLCAttempt structs using the pre-loaded
	// batch data.
	attempts := make([]HTLCAttempt, 0, len(dbAttempts))
	for _, dbAttempt := range dbAttempts {
		attemptIndex := dbAttempt.AttemptIndex
		// Convert the batch row type to the single row type.
		attempt, err := dbAttemptToHTLCAttempt(
			dbAttempt, batchData.hopsByAttempt[attemptIndex],
			batchData.hopCustomRecords,
			batchData.routeCustomRecords[attemptIndex],
		)
		if err != nil {
			return nil, fmt.Errorf("failed to convert attempt "+
				"%d: %w", attemptIndex, err)
		}
		attempts = append(attempts, *attempt)
	}

	// Set the failure reason if present.
	//
	// TODO(ziggie): Rename it to Payment Memo in the database?
	var failureReason *FailureReason
	if payment.FailReason.Valid {
		reason := FailureReason(payment.FailReason.Int32)
		failureReason = &reason
	}

	mpPayment := &MPPayment{
		SequenceNum:   uint64(payment.ID),
		Info:          info,
		HTLCs:         attempts,
		FailureReason: failureReason,
	}

	// The status and state will be determined by calling
	// SetState after construction.
	if err := mpPayment.SetState(); err != nil {
		return nil, fmt.Errorf("failed to set payment state: %w", err)
	}

	return mpPayment, nil
}

// QueryPayments queries and retrieves payments from the database with support
// for filtering, pagination, and efficient batch loading of related data.
//
// The function accepts a Query parameter that controls:
//   - Pagination: IndexOffset specifies where to start (exclusive), and
//     MaxPayments limits the number of results returned
//   - Ordering: Reversed flag determines if results are returned in reverse
//     chronological order
//   - Filtering: CreationDateStart/End filter by creation time, and
//     IncludeIncomplete controls whether non-succeeded payments are included
//   - Metadata: CountTotal flag determines if the total payment count should
//     be calculated
//
// The function optimizes performance by loading all related data (HTLCs,
// sequences, failure reasons, etc.) for multiple payments in a single batch
// query, rather than fetching each payment's data individually.
//
// Returns a Response containing:
//   - Payments: the list of matching payments with complete data
//   - FirstIndexOffset/LastIndexOffset: pagination cursors for the first and
//     last payment in the result set
//   - TotalCount: total number of payments in the database (if CountTotal was
//     requested, otherwise 0)
//
// This is part of the DB interface.
func (s *SQLStore) QueryPayments(ctx context.Context, query Query) (Response,
	error) {

	if query.MaxPayments == 0 {
		return Response{}, fmt.Errorf("max payments must be non-zero")
	}

	var (
		allPayments   []*MPPayment
		totalCount    int64
		initialCursor int64
	)

	extractCursor := func(
		row sqlc.FilterPaymentsRow) int64 {

		return row.Payment.ID
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

		// collectFunc extracts the payment ID from each payment row.
		collectFunc := func(row sqlc.FilterPaymentsRow) (int64,
			error) {

			return row.Payment.ID, nil
		}

		// batchDataFunc loads all related data for a batch of payments.
		batchDataFunc := func(ctx context.Context, paymentIDs []int64) (
			*paymentsBatchData, error) {

			return s.loadPaymentsBatchData(ctx, db, paymentIDs)
		}

		// processPayment processes each payment with the batch-loaded
		// data.
		processPayment := func(ctx context.Context,
			dbPayment sqlc.FilterPaymentsRow,
			batchData *paymentsBatchData) error {

			// Build the payment from the pre-loaded batch data.
			mpPayment, err := s.buildPaymentFromBatchData(
				dbPayment, batchData,
			)
			if err != nil {
				return fmt.Errorf("failed to fetch payment "+
					"with complete data: %w", err)
			}

			// To keep compatibility with the old API, we only
			// return non-succeeded payments if requested.
			if mpPayment.Status != StatusSucceeded &&
				!query.IncludeIncomplete {

				return nil
			}

			if uint64(len(allPayments)) >= query.MaxPayments {
				return errMaxPaymentsReached
			}

			allPayments = append(allPayments, mpPayment)

			return nil
		}

		queryFunc := func(ctx context.Context, lastID int64,
			limit int32) ([]sqlc.FilterPaymentsRow, error) {

			filterParams := sqlc.FilterPaymentsParams{
				NumLimit: limit,
				Reverse:  query.Reversed,
				// For now there only BOLT 11 payment intents
				// exist.
				IntentType: sqldb.SQLInt16(
					PaymentIntentTypeBolt11,
				),
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

			return db.FilterPayments(ctx, filterParams)
		}

		if query.Reversed {
			if query.IndexOffset == 0 {
				initialCursor = int64(math.MaxInt64)
			} else {
				initialCursor = int64(query.IndexOffset)
			}
		} else {
			initialCursor = int64(query.IndexOffset)
		}

		return sqldb.ExecuteCollectAndBatchWithSharedDataQuery(
			ctx, s.cfg.QueryCfg, initialCursor, queryFunc,
			extractCursor, collectFunc, batchDataFunc,
			processPayment,
		)
	}, func() {
		allPayments = nil
	})

	// We make sure we don't return an error if we reached the maximum
	// number of payments. Which is the pagination limit for the query
	// itself.
	if err != nil && !errors.Is(err, errMaxPaymentsReached) {
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

	// If the query was reversed, we need to reverse the payment list
	// to match the kvstore behavior and return payments in forward order.
	if query.Reversed {
		for i, j := 0, len(allPayments)-1; i < j; i, j = i+1, j-1 {
			allPayments[i], allPayments[j] = allPayments[j],
				allPayments[i]
		}
	}

	return Response{
		Payments:         allPayments,
		FirstIndexOffset: allPayments[0].SequenceNum,
		LastIndexOffset:  allPayments[len(allPayments)-1].SequenceNum,
		TotalCount:       uint64(totalCount),
	}, nil
}

// FetchPayment retrieves a complete payment record from the database by its
// payment hash. The returned MPPayment includes all payment metadata such as
// creation info, payment status, current state, all HTLC attempts (both
// successful and failed), and the failure reason if the payment has been
// marked as failed.
//
// Returns ErrPaymentNotInitiated if no payment with the given hash exists.
//
// This is part of the DB interface.
func (s *SQLStore) FetchPayment(paymentHash lntypes.Hash) (*MPPayment, error) {
	ctx := context.TODO()

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("failed to fetch payment: %w", err)
		}

		if errors.Is(err, sql.ErrNoRows) {
			return ErrPaymentNotInitiated
		}

		mpPayment, err = s.fetchPaymentWithCompleteData(
			ctx, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return nil, err
	}

	return mpPayment, nil
}
