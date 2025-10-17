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
//nolint:ll,interfacebloat
type SQLQueries interface {
	/*
		Payment DB read operations.
	*/
	FilterPayments(ctx context.Context, query sqlc.FilterPaymentsParams) ([]sqlc.FilterPaymentsRow, error)
	FetchPayment(ctx context.Context, paymentIdentifier []byte) (sqlc.FetchPaymentRow, error)
	FetchPaymentsByIDs(ctx context.Context, paymentIDs []int64) ([]sqlc.FetchPaymentsByIDsRow, error)

	CountPayments(ctx context.Context) (int64, error)

	FetchHtlcAttemptsForPayment(ctx context.Context, paymentID int64) ([]sqlc.FetchHtlcAttemptsForPaymentRow, error)
	FetchAllInflightAttempts(ctx context.Context) ([]sqlc.PaymentHtlcAttempt, error)
	FetchHopsForAttempt(ctx context.Context, htlcAttemptIndex int64) ([]sqlc.FetchHopsForAttemptRow, error)
	FetchHopsForAttempts(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.FetchHopsForAttemptsRow, error)

	FetchPaymentLevelFirstHopCustomRecords(ctx context.Context, paymentID int64) ([]sqlc.PaymentFirstHopCustomRecord, error)
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
func (s *SQLStore) fetchPaymentWithCompleteData(ctx context.Context,
	db SQLQueries, dbPayment sqlc.PaymentAndIntent) (*MPPayment, error) {

	// The query will only return BOLT 11 payment intents or intents with
	// no intent type set.
	paymentIntent := dbPayment.GetPaymentIntent()
	paymentRequest := paymentIntent.IntentPayload

	// Fetch payment-level first hop custom records.
	payment := dbPayment.GetPayment()
	customRecords, err := db.FetchPaymentLevelFirstHopCustomRecords(
		ctx, payment.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch payment level custom "+
			"records: %w", err)
	}

	// Convert to the FirstHopCustomRecords map.
	var firstHopCustomRecords lnwire.CustomRecords
	if len(customRecords) > 0 {
		firstHopCustomRecords = make(lnwire.CustomRecords)
		for _, record := range customRecords {
			firstHopCustomRecords[uint64(record.Key)] = record.Value
		}
	}

	// Convert the basic payment info.
	info := dbPaymentToCreationInfo(
		payment.PaymentIdentifier, payment.AmountMsat,
		payment.CreatedAt, paymentRequest, firstHopCustomRecords,
	)

	// Fetch all HTLC attempts for this payment.
	attempts, err := s.fetchHTLCAttemptsForPayment(
		ctx, db, payment.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch HTLC attempts: %w",
			err)
	}

	// Set the failure reason if present.
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

// fetchHTLCAttemptsForPayment fetches all HTLC attempts for a payment and
// uses ExecuteBatchQuery to efficiently fetch hops and custom records.
func (s *SQLStore) fetchHTLCAttemptsForPayment(ctx context.Context,
	db SQLQueries, paymentID int64) ([]HTLCAttempt, error) {

	// Fetch all HTLC attempts for this payment.
	dbAttempts, err := db.FetchHtlcAttemptsForPayment(
		ctx, paymentID,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to fetch HTLC attempts: %w",
			err)
	}

	if len(dbAttempts) == 0 {
		return nil, nil
	}

	// Collect all attempt indices for batch fetching.
	attemptIndices := make([]int64, len(dbAttempts))
	for i, attempt := range dbAttempts {
		attemptIndices[i] = attempt.AttemptIndex
	}

	// Fetch all hops for all attempts using ExecuteBatchQuery.
	hopsByAttempt := make(map[int64][]sqlc.FetchHopsForAttemptsRow)
	err = sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, attemptIndices,
		func(idx int64) int64 { return idx },
		func(ctx context.Context, indices []int64) (
			[]sqlc.FetchHopsForAttemptsRow, error) {

			return db.FetchHopsForAttempts(ctx, indices)
		},
		func(ctx context.Context,
			hop sqlc.FetchHopsForAttemptsRow) error {

			hopsByAttempt[hop.HtlcAttemptIndex] = append(
				hopsByAttempt[hop.HtlcAttemptIndex], hop,
			)

			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch hops for attempts: %w",
			err)
	}

	// Collect all hop IDs for fetching hop-level custom records.
	var hopIDs []int64
	for _, hops := range hopsByAttempt {
		for _, hop := range hops {
			hopIDs = append(hopIDs, hop.ID)
		}
	}

	// Fetch all hop-level custom records using ExecuteBatchQuery.
	hopCustomRecords := make(map[int64][]sqlc.PaymentHopCustomRecord)
	if len(hopIDs) > 0 {
		err = sqldb.ExecuteBatchQuery(
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

				hopCustomRecords[record.HopID] = append(
					hopCustomRecords[record.HopID], record,
				)

				return nil
			},
		)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch hop custom "+
				"records: %w", err)
		}
	}

	// Fetch route-level first hop custom records using ExecuteBatchQuery.
	routeCustomRecords := make(
		map[int64][]sqlc.PaymentAttemptFirstHopCustomRecord,
	)
	err = sqldb.ExecuteBatchQuery(
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

			routeCustomRecords[record.HtlcAttemptIndex] = append(
				routeCustomRecords[record.HtlcAttemptIndex],
				record,
			)

			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch route custom "+
			"records: %w", err)
	}

	// Now convert all attempts to HTLCAttempt structs.
	attempts := make([]HTLCAttempt, 0, len(dbAttempts))
	for _, dbAttempt := range dbAttempts {
		attemptIndex := dbAttempt.AttemptIndex
		attempt, err := dbAttemptToHTLCAttempt(
			dbAttempt, hopsByAttempt[attemptIndex],
			hopCustomRecords,
			routeCustomRecords[attemptIndex],
		)
		if err != nil {
			return nil, fmt.Errorf("failed to convert attempt "+
				"%d: %w", attemptIndex, err)
		}
		attempts = append(attempts, *attempt)
	}

	return attempts, nil
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

		processPayment := func(ctx context.Context,
			dbPayment sqlc.FilterPaymentsRow) error {

			// Fetch all the additional data for the payment.
			mpPayment, err := s.fetchPaymentWithCompleteData(
				ctx, db, dbPayment,
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

		return sqldb.ExecutePaginatedQuery(
			ctx, s.cfg.QueryCfg, initialCursor, queryFunc,
			extractCursor, processPayment,
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

// FetchPayment fetches the payment corresponding to the given payment
// hash.
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
	}, func() {
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch payment: %w", err)
	}

	return mpPayment, nil
}
