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

	FetchHtlcAttemptsForPayments(ctx context.Context, paymentIDs []int64) ([]sqlc.FetchHtlcAttemptsForPaymentsRow, error)
	FetchHtlcAttemptResolutionsForPayments(ctx context.Context, paymentIDs []int64) ([]sqlc.FetchHtlcAttemptResolutionsForPaymentsRow, error)
	FetchAllInflightAttempts(ctx context.Context, arg sqlc.FetchAllInflightAttemptsParams) ([]sqlc.PaymentHtlcAttempt, error)
	FetchHopsForAttempts(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.FetchHopsForAttemptsRow, error)

	FetchPaymentLevelFirstHopCustomRecords(ctx context.Context, paymentIDs []int64) ([]sqlc.PaymentFirstHopCustomRecord, error)
	FetchRouteLevelFirstHopCustomRecords(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.PaymentAttemptFirstHopCustomRecord, error)
	FetchHopLevelCustomRecords(ctx context.Context, hopIDs []int64) ([]sqlc.PaymentHopCustomRecord, error)

	/*
		Payment DB write operations.
	*/
	InsertPaymentIntent(ctx context.Context, arg sqlc.InsertPaymentIntentParams) (int64, error)
	InsertPayment(ctx context.Context, arg sqlc.InsertPaymentParams) (int64, error)
	InsertPaymentFirstHopCustomRecord(ctx context.Context, arg sqlc.InsertPaymentFirstHopCustomRecordParams) error

	InsertHtlcAttempt(ctx context.Context, arg sqlc.InsertHtlcAttemptParams) (int64, error)
	InsertRouteHop(ctx context.Context, arg sqlc.InsertRouteHopParams) (int64, error)
	InsertRouteHopMpp(ctx context.Context, arg sqlc.InsertRouteHopMppParams) error
	InsertRouteHopAmp(ctx context.Context, arg sqlc.InsertRouteHopAmpParams) error
	InsertRouteHopBlinded(ctx context.Context, arg sqlc.InsertRouteHopBlindedParams) error

	InsertPaymentAttemptFirstHopCustomRecord(ctx context.Context, arg sqlc.InsertPaymentAttemptFirstHopCustomRecordParams) error
	InsertPaymentHopCustomRecord(ctx context.Context, arg sqlc.InsertPaymentHopCustomRecordParams) error

	SettleAttempt(ctx context.Context, arg sqlc.SettleAttemptParams) error
	FailAttempt(ctx context.Context, arg sqlc.FailAttemptParams) error

	FailPayment(ctx context.Context, arg sqlc.FailPaymentParams) (sql.Result, error)

	DeletePayment(ctx context.Context, paymentID int64) error

	// DeleteFailedAttempts removes all failed HTLCs from the db for a
	// given payment.
	DeleteFailedAttempts(ctx context.Context, paymentID int64) error
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
		cfg: cfg,
		db:  db,
	}, nil
}

// A compile-time constraint to ensure SQLStore implements DB.
var _ DB = (*SQLStore)(nil)

// fetchPaymentWithCompleteData fetches a payment with all its related data
// including attempts, hops, and custom records from the database.
// This is a convenience wrapper around the batch loading functions for single
// payment operations.
func fetchPaymentWithCompleteData(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries,
	dbPayment sqlc.PaymentAndIntent) (*MPPayment, error) {

	payment := dbPayment.GetPayment()

	// Load batch data for this single payment.
	batchData, err := batchLoadPaymentDetailsData(
		ctx, cfg, db, []int64{payment.ID},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load batch data: %w", err)
	}

	// Build the payment from the batch data.
	return buildPaymentFromBatchData(dbPayment, batchData)
}

// paymentsCompleteData holds the full payment data when batch loading base
// payment data and all the related data for a payment.
type paymentsCompleteData struct {
	*paymentsBaseData
	*paymentsDetailsData
}

// batchLoadPayments loads the full payment data for a batch of payment IDs.
func batchLoadPayments(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, paymentIDs []int64) (*paymentsCompleteData, error) {

	baseData, err := batchLoadpaymentsBaseData(ctx, cfg, db, paymentIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to load payment base data: %w",
			err)
	}

	batchData, err := batchLoadPaymentDetailsData(ctx, cfg, db, paymentIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to load payment batch data: %w",
			err)
	}

	return &paymentsCompleteData{
		paymentsBaseData:    baseData,
		paymentsDetailsData: batchData,
	}, nil
}

// paymentsBaseData holds the base payment and intent data for a batch of
// payments.
type paymentsBaseData struct {
	// paymentsAndIntents maps payment ID to its payment and intent data.
	paymentsAndIntents map[int64]sqlc.PaymentAndIntent
}

// batchLoadpaymentsBaseData loads the base payment and payment intent data for
// a batch of payment IDs. This complements loadPaymentsBatchData which loads
// related data (attempts, hops, custom records) but not the payment table
// and payment intent table data.
func batchLoadpaymentsBaseData(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries,
	paymentIDs []int64) (*paymentsBaseData, error) {

	baseData := &paymentsBaseData{
		paymentsAndIntents: make(map[int64]sqlc.PaymentAndIntent),
	}

	if len(paymentIDs) == 0 {
		return baseData, nil
	}

	err := sqldb.ExecuteBatchQuery(
		ctx, cfg, paymentIDs,
		func(id int64) int64 { return id },
		func(ctx context.Context, ids []int64) (
			[]sqlc.FetchPaymentsByIDsRow, error) {

			records, err := db.FetchPaymentsByIDs(
				ctx, ids,
			)

			return records, err
		},
		func(ctx context.Context,
			payment sqlc.FetchPaymentsByIDsRow) error {

			baseData.paymentsAndIntents[payment.ID] = payment

			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch payment base "+
			"data: %w", err)
	}

	return baseData, nil
}

// paymentsRelatedData holds all the batch-loaded data for multiple payments.
// This does not include the base payment and intent data which is fetched
// separately. It includes the additional data like attempts, hops, hop custom
// records, and route custom records.
type paymentsDetailsData struct {
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

// batchLoadPaymentCustomRecords loads payment-level custom records for a given
// set of payment IDs. It uses a batch query to fetch all custom records for
// the given payment IDs.
func batchLoadPaymentCustomRecords(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries, paymentIDs []int64,
	batchData *paymentsDetailsData) error {

	return sqldb.ExecuteBatchQuery(
		ctx, cfg, paymentIDs,
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

// batchLoadHtlcAttempts loads HTLC attempts for all payments and returns all
// attempt indices. It uses a batch query to fetch all attempts for the given
// payment IDs.
func batchLoadHtlcAttempts(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, paymentIDs []int64,
	batchData *paymentsDetailsData) ([]int64, error) {

	var allAttemptIndices []int64

	err := sqldb.ExecuteBatchQuery(
		ctx, cfg, paymentIDs,
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

// batchLoadHopsForAttempts loads hops for all attempts and returns all hop IDs.
// It uses a batch query to fetch all hops for the given attempt indices.
func batchLoadHopsForAttempts(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, attemptIndices []int64,
	batchData *paymentsDetailsData) ([]int64, error) {

	var hopIDs []int64

	err := sqldb.ExecuteBatchQuery(
		ctx, cfg, attemptIndices,
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

// batchLoadHopCustomRecords loads hop-level custom records for all hops. It
// uses a batch query to fetch all custom records for the given hop IDs.
func batchLoadHopCustomRecords(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, hopIDs []int64, batchData *paymentsDetailsData) error {

	return sqldb.ExecuteBatchQuery(
		ctx, cfg, hopIDs,
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

// batchLoadRouteCustomRecords loads route-level first hop custom records for
// all attempts. It uses a batch query to fetch all custom records for the given
// attempt indices.
func batchLoadRouteCustomRecords(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, attemptIndices []int64,
	batchData *paymentsDetailsData) error {

	return sqldb.ExecuteBatchQuery(
		ctx, cfg, attemptIndices,
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

// paymentStatusData holds lightweight resolution data for computing
// payment status efficiently during deletion operations.
type paymentStatusData struct {
	// resolutionTypes maps payment ID to a list of resolution types
	// for that payment's HTLC attempts.
	resolutionTypes map[int64][]sql.NullInt32
}

// batchLoadPaymentResolutions loads only HTLC resolution types for multiple
// payments. This is a lightweight alternative to batchLoadPaymentsRelatedData
// that's optimized for operations that only need to determine payment status.
func batchLoadPaymentResolutions(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, paymentIDs []int64) (*paymentStatusData, error) {

	batchStatusData := &paymentStatusData{
		resolutionTypes: make(map[int64][]sql.NullInt32),
	}

	if len(paymentIDs) == 0 {
		return batchStatusData, nil
	}

	// Use a batch query to fetch all resolution types for the given payment
	// IDs.
	err := sqldb.ExecuteBatchQuery(
		ctx, cfg, paymentIDs,
		func(id int64) int64 { return id },
		func(ctx context.Context, ids []int64) (
			[]sqlc.FetchHtlcAttemptResolutionsForPaymentsRow,
			error) {

			return db.FetchHtlcAttemptResolutionsForPayments(
				ctx, ids,
			)
		},
		//nolint:ll
		func(ctx context.Context,
			res sqlc.FetchHtlcAttemptResolutionsForPaymentsRow) error {

			// Group resolutions by payment ID.
			batchStatusData.resolutionTypes[res.PaymentID] = append(
				batchStatusData.resolutionTypes[res.PaymentID],
				res.ResolutionType,
			)

			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch HTLC resolutions: %w",
			err)
	}

	return batchStatusData, nil
}

// loadPaymentResolutions is a single-payment wrapper around
// batchLoadPaymentResolutions for convenience and to prevent duplicate queries
// so we reuse the same batch query for all payments.
func loadPaymentResolutions(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, paymentID int64) ([]sql.NullInt32, error) {

	batchData, err := batchLoadPaymentResolutions(
		ctx, cfg, db, []int64{paymentID},
	)
	if err != nil {
		return nil, err
	}

	return batchData.resolutionTypes[paymentID], nil
}

// computePaymentStatusFromResolutions determines the payment status from
// resolution types and failure reason without building the complete MPPayment
// structure. This is a lightweight version that builds minimal HTLCAttempt
// structures and delegates to decidePaymentStatus for consistency.
func computePaymentStatusFromResolutions(resolutionTypes []sql.NullInt32,
	failReason sql.NullInt32) (PaymentStatus, error) {

	// Build minimal HTLCAttempt slice with only resolution info.
	htlcs := make([]HTLCAttempt, len(resolutionTypes))
	for i, resType := range resolutionTypes {
		if !resType.Valid {
			// NULL resolution_type means in-flight (no Settle, no
			// Failure).
			continue
		}

		switch HTLCAttemptResolutionType(resType.Int32) {
		case HTLCAttemptResolutionSettled:
			// Mark as settled (preimage details not needed for
			// status).
			htlcs[i].Settle = &HTLCSettleInfo{}

		case HTLCAttemptResolutionFailed:
			// Mark as failed (failure details not needed for
			// status).
			htlcs[i].Failure = &HTLCFailInfo{}

		default:
			return 0, fmt.Errorf("unknown resolution type: %v",
				resType.Int32)
		}
	}

	// Convert fail reason to FailureReason pointer.
	var failureReason *FailureReason
	if failReason.Valid {
		reason := FailureReason(failReason.Int32)
		failureReason = &reason
	}

	// Use the existing status decision logic.
	return decidePaymentStatus(htlcs, failureReason)
}

// batchLoadPaymentDetailsData loads all related data for multiple payments in
// batch. It uses a batch queries to fetch all data for the given payment IDs.
func batchLoadPaymentDetailsData(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, paymentIDs []int64) (*paymentsDetailsData, error) {

	batchData := &paymentsDetailsData{
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
	err := batchLoadPaymentCustomRecords(
		ctx, cfg, db, paymentIDs, batchData,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch payment custom "+
			"records: %w", err)
	}

	// Load HTLC attempts and collect attempt indices.
	allAttemptIndices, err := batchLoadHtlcAttempts(
		ctx, cfg, db, paymentIDs, batchData,
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
	hopIDs, err := batchLoadHopsForAttempts(
		ctx, cfg, db, allAttemptIndices, batchData,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch hops for attempts: %w",
			err)
	}

	// Load hop-level custom records if there are any hops.
	if len(hopIDs) > 0 {
		err = batchLoadHopCustomRecords(ctx, cfg, db, hopIDs, batchData)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch hop custom "+
				"records: %w", err)
		}
	}

	// Load route-level first hop custom records.
	err = batchLoadRouteCustomRecords(
		ctx, cfg, db, allAttemptIndices, batchData,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch route custom "+
			"records: %w", err)
	}

	return batchData, nil
}

// buildPaymentFromBatchData builds a complete MPPayment from a database payment
// and pre-loaded batch data.
func buildPaymentFromBatchData(dbPayment sqlc.PaymentAndIntent,
	batchData *paymentsDetailsData) (*MPPayment, error) {

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

	extractCursor := func(row sqlc.FilterPaymentsRow) int64 {
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
		collectFunc := func(row sqlc.FilterPaymentsRow) (int64, error) {
			return row.Payment.ID, nil
		}

		// batchDataFunc loads all related data for a batch of payments.
		batchDataFunc := func(ctx context.Context, paymentIDs []int64) (
			*paymentsDetailsData, error) {

			return batchLoadPaymentDetailsData(
				ctx, s.cfg.QueryCfg, db, paymentIDs,
			)
		}

		// processPayment processes each payment with the batch-loaded
		// data.
		processPayment := func(ctx context.Context,
			dbPayment sqlc.FilterPaymentsRow,
			batchData *paymentsDetailsData) error {

			// Build the payment from the pre-loaded batch data.
			mpPayment, err := buildPaymentFromBatchData(
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

// fetchPaymentByHash fetches a payment by its hash from the database. It is a
// convenience wrapper around the FetchPayment method and checks for
// no rows error and returns ErrPaymentNotInitiated if no payment is found.
func fetchPaymentByHash(ctx context.Context, db SQLQueries,
	paymentHash lntypes.Hash) (sqlc.FetchPaymentRow, error) {

	dbPayment, err := db.FetchPayment(ctx, paymentHash[:])
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return dbPayment, fmt.Errorf("failed to fetch payment: %w", err)
	}

	if errors.Is(err, sql.ErrNoRows) {
		return dbPayment, ErrPaymentNotInitiated
	}

	return dbPayment, nil
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
func (s *SQLStore) FetchPayment(ctx context.Context,
	paymentHash lntypes.Hash) (*MPPayment, error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbPayment, err := fetchPaymentByHash(ctx, db, paymentHash)
		if err != nil {
			return err
		}

		mpPayment, err = fetchPaymentWithCompleteData(
			ctx, s.cfg.QueryCfg, db, dbPayment,
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

// FetchInFlightPayments retrieves all payments that have HTLC attempts
// currently in flight (not yet settled or failed). These are payments with at
// least one HTLC attempt that has been registered but has no resolution record.
//
// The SQLStore implementation provides a significant performance improvement
// over the KVStore implementation by using targeted SQL queries instead of
// scanning all payments.
//
// This method is part of the PaymentReader interface, which is embedded in the
// DB interface. It's typically called during node startup to resume monitoring
// of pending payments and ensure HTLCs are properly tracked.
//
// TODO(ziggie): Consider changing the interface to use a callback or iterator
// pattern instead of returning all payments at once. This would allow
// processing payments one at a time without holding them all in memory
// simultaneously:
//   - Callback: func FetchInFlightPayments(ctx, func(*MPPayment) error) error
//   - Iterator: func FetchInFlightPayments(ctx) (PaymentIterator, error)
//
// While inflight payments are typically a small subset, this would improve
// memory efficiency for nodes with unusually high numbers of concurrent
// payments and would better leverage the existing pagination infrastructure.
func (s *SQLStore) FetchInFlightPayments(ctx context.Context) ([]*MPPayment,
	error) {

	var mpPayments []*MPPayment

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		// Track which payment IDs we've already processed across all
		// pages to avoid loading the same payment multiple times when
		// multiple inflight attempts belong to the same payment.
		processedPayments := make(map[int64]*MPPayment)

		extractCursor := func(row sqlc.PaymentHtlcAttempt) int64 {
			return row.AttemptIndex
		}

		// collectFunc extracts the payment ID from each attempt row.
		collectFunc := func(row sqlc.PaymentHtlcAttempt) (
			int64, error) {

			return row.PaymentID, nil
		}

		// batchDataFunc loads payment data for a batch of payment IDs,
		// but only for IDs we haven't processed yet.
		batchDataFunc := func(ctx context.Context,
			paymentIDs []int64) (*paymentsCompleteData, error) {

			// Filter out already-processed payment IDs.
			uniqueIDs := make([]int64, 0, len(paymentIDs))
			for _, id := range paymentIDs {
				_, processed := processedPayments[id]
				if !processed {
					uniqueIDs = append(uniqueIDs, id)
				}
			}

			// If uniqueIDs is empty, the batch load will return
			// empty batch data.
			return batchLoadPayments(
				ctx, s.cfg.QueryCfg, db, uniqueIDs,
			)
		}

		// processAttempt processes each attempt. We only build and
		// store the payment once per unique payment ID.
		processAttempt := func(ctx context.Context,
			row sqlc.PaymentHtlcAttempt,
			batchData *paymentsCompleteData) error {

			// Skip if we've already processed this payment.
			_, processed := processedPayments[row.PaymentID]
			if processed {
				return nil
			}

			dbPayment := batchData.paymentsAndIntents[row.PaymentID]

			// Build the payment from batch data.
			mpPayment, err := buildPaymentFromBatchData(
				dbPayment, batchData.paymentsDetailsData,
			)
			if err != nil {
				return fmt.Errorf("failed to build payment: %w",
					err)
			}

			// Store in our processed map.
			processedPayments[row.PaymentID] = mpPayment

			return nil
		}

		queryFunc := func(ctx context.Context, lastAttemptIndex int64,
			limit int32) ([]sqlc.PaymentHtlcAttempt,
			error) {

			return db.FetchAllInflightAttempts(ctx,
				sqlc.FetchAllInflightAttemptsParams{
					AttemptIndex: lastAttemptIndex,
					Limit:        limit,
				},
			)
		}

		err := sqldb.ExecuteCollectAndBatchWithSharedDataQuery(
			ctx, s.cfg.QueryCfg, int64(-1), queryFunc,
			extractCursor, collectFunc, batchDataFunc,
			processAttempt,
		)
		if err != nil {
			return err
		}

		// Convert map to slice.
		mpPayments = make([]*MPPayment, 0, len(processedPayments))
		for _, payment := range processedPayments {
			mpPayments = append(mpPayments, payment)
		}

		return nil
	}, func() {
		mpPayments = nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch inflight "+
			"payments: %w", err)
	}

	return mpPayments, nil
}

// DeleteFailedAttempts removes all failed HTLC attempts from the database for
// the specified payment, while preserving the payment record itself and any
// successful or in-flight attempts.
//
// The method performs the following validations before deletion:
//   - StatusInitiated: Can delete failed attempts
//   - StatusInFlight: Cannot delete, returns ErrPaymentInFlight (active HTLCs
//     still on the network)
//   - StatusSucceeded: Can delete failed attempts (payment completed)
//   - StatusFailed: Can delete failed attempts (payment permanently failed)
//
// This method is idempotent - calling it multiple times on the same payment
// has no adverse effects.
//
// This method is part of the PaymentControl interface, which is embedded in
// the PaymentWriter interface and ultimately the DB interface. It represents
// the final step (step 5) in the payment lifecycle control flow and should be
// called after a payment reaches a terminal state (succeeded or permanently
// failed) to clean up historical failed attempts.
func (s *SQLStore) DeleteFailedAttempts(ctx context.Context,
	paymentHash lntypes.Hash) error {

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		dbPayment, err := fetchPaymentByHash(ctx, db, paymentHash)
		if err != nil {
			return err
		}

		paymentStatus, err := computePaymentStatusFromDB(
			ctx, s.cfg.QueryCfg, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to compute payment "+
				"status: %w", err)
		}

		if err := paymentStatus.removable(); err != nil {
			return fmt.Errorf("cannot delete failed "+
				"attempts for payment %v: %w", paymentHash, err)
		}

		// Then we delete the failed attempts for this payment.
		return db.DeleteFailedAttempts(ctx, dbPayment.GetPayment().ID)
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("failed to delete failed attempts for "+
			"payment %v: %w", paymentHash, err)
	}

	return nil
}

// computePaymentStatusFromDB computes the payment status by fetching minimal
// data from the database. This is a lightweight query optimized for SQL that
// doesn't load route data, making it significantly more efficient than
// FetchPayment when only the status is needed.
func computePaymentStatusFromDB(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, dbPayment sqlc.PaymentAndIntent) (PaymentStatus, error) {

	payment := dbPayment.GetPayment()

	// Load the resolution types for the payment.
	resolutionTypes, err := loadPaymentResolutions(
		ctx, cfg, db, payment.ID,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to load payment resolutions: %w",
			err)
	}

	// Use the lightweight status computation.
	status, err := computePaymentStatusFromResolutions(
		resolutionTypes, payment.FailReason,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to compute payment status: %w",
			err)
	}

	return status, nil
}

// DeletePayment removes a payment or its failed HTLC attempts from the
// database based on the failedAttemptsOnly flag.
//
// If failedAttemptsOnly is true, this method deletes only the failed HTLC
// attempts for the payment while preserving the payment record itself and any
// successful or in-flight attempts. This is useful for cleaning up historical
// failed attempts after a payment reaches a terminal state.
//
// If failedAttemptsOnly is false, this method deletes the entire payment
// record including all payment metadata, payment creation info, all HTLC
// attempts (both failed and successful), and associated data such as payment
// intents and custom records.
//
// Before deletion, this method validates the payment status to ensure it's
// safe to delete:
//   - StatusInitiated: Can be deleted (no HTLCs sent yet)
//   - StatusInFlight: Cannot be deleted, returns ErrPaymentInFlight (active
//     HTLCs on the network)
//   - StatusSucceeded: Can be deleted (payment completed successfully)
//   - StatusFailed: Can be deleted (payment has failed permanently)
//
// Returns an error if the payment has in-flight HTLCs or if the payment
// doesn't exist.
//
// This method is part of the PaymentWriter interface, which is embedded in
// the DB interface.
func (s *SQLStore) DeletePayment(ctx context.Context, paymentHash lntypes.Hash,
	failedHtlcsOnly bool) error {

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		dbPayment, err := fetchPaymentByHash(ctx, db, paymentHash)
		if err != nil {
			return err
		}

		paymentStatus, err := computePaymentStatusFromDB(
			ctx, s.cfg.QueryCfg, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to compute payment "+
				"status: %w", err)
		}

		if err := paymentStatus.removable(); err != nil {
			return fmt.Errorf("payment %v cannot be deleted: %w",
				paymentHash, err)
		}

		// If we are only deleting failed HTLCs, we delete them.
		if failedHtlcsOnly {
			return db.DeleteFailedAttempts(
				ctx, dbPayment.GetPayment().ID,
			)
		}

		// In case we are not deleting failed HTLCs, we delete the
		// payment which will cascade delete all related data.
		return db.DeletePayment(ctx, dbPayment.GetPayment().ID)
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("failed to delete failed attempts for "+
			"payment %v: %w", paymentHash, err)
	}

	return nil
}

// InitPayment creates a new payment record in the database with the given
// payment hash and creation info.
//
// Before creating the payment, this method checks if a payment with the same
// hash already exists and validates whether initialization is allowed based on
// the existing payment's status:
//   - StatusInitiated: Returns ErrPaymentExists (payment already created,
//     HTLCs may be in flight)
//   - StatusInFlight: Returns ErrPaymentInFlight (payment currently being
//     attempted)
//   - StatusSucceeded: Returns ErrAlreadyPaid (payment already succeeded)
//   - StatusFailed: Allows retry by deleting the old payment record and
//     creating a new one
//
// If no existing payment is found, a new payment record is created with
// StatusInitiated and stored with all associated metadata.
//
// This method is part of the PaymentControl interface, which is embedded in
// the PaymentWriter interface and ultimately the DB interface, representing
// the first step in the payment lifecycle control flow.
func (s *SQLStore) InitPayment(ctx context.Context, paymentHash lntypes.Hash,
	paymentCreationInfo *PaymentCreationInfo) error {

	// Create the payment in the database.
	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		existingPayment, err := db.FetchPayment(ctx, paymentHash[:])
		switch {
		// A payment with this hash already exists. We need to check its
		// status to see if we can re-initialize.
		case err == nil:
			paymentStatus, err := computePaymentStatusFromDB(
				ctx, s.cfg.QueryCfg, db, existingPayment,
			)
			if err != nil {
				return fmt.Errorf("failed to compute payment "+
					"status: %w", err)
			}

			// Check if the payment is initializable otherwise
			// we'll return early.
			if err := paymentStatus.initializable(); err != nil {
				return fmt.Errorf("payment is not "+
					"initializable: %w", err)
			}

			// If the initializable check above passes, then the
			// existing payment has failed. So we delete it and
			// all of its previous artifacts. We rely on
			// cascading deletes to clean up the rest.
			err = db.DeletePayment(ctx, existingPayment.Payment.ID)
			if err != nil {
				return fmt.Errorf("failed to delete "+
					"payment: %w", err)
			}

		// An unexpected error occurred while fetching the payment.
		case !errors.Is(err, sql.ErrNoRows):
			// Some other error occurred
			return fmt.Errorf("failed to check existing "+
				"payment: %w", err)

		// The payment does not yet exist, so we can proceed.
		default:
		}

		// Insert the payment first to get its ID.
		paymentID, err := db.InsertPayment(
			ctx, sqlc.InsertPaymentParams{
				AmountMsat: int64(
					paymentCreationInfo.Value,
				),
				CreatedAt: paymentCreationInfo.
					CreationTime.UTC(),
				PaymentIdentifier: paymentHash[:],
			},
		)
		if err != nil {
			return fmt.Errorf("failed to insert payment: %w", err)
		}

		// If there's a payment request, insert the payment intent.
		if len(paymentCreationInfo.PaymentRequest) > 0 {
			_, err = db.InsertPaymentIntent(
				ctx, sqlc.InsertPaymentIntentParams{
					PaymentID: paymentID,
					IntentType: int16(
						PaymentIntentTypeBolt11,
					),
					IntentPayload: paymentCreationInfo.
						PaymentRequest,
				},
			)
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"payment intent: %w", err)
			}
		}

		firstHopCustomRecords := paymentCreationInfo.
			FirstHopCustomRecords

		for key, value := range firstHopCustomRecords {
			err = db.InsertPaymentFirstHopCustomRecord(
				ctx,
				sqlc.InsertPaymentFirstHopCustomRecordParams{
					PaymentID: paymentID,
					Key:       int64(key),
					Value:     value,
				},
			)
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"payment first hop custom "+
					"record: %w", err)
			}
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("failed to initialize payment: %w", err)
	}

	return nil
}

// insertRouteHops inserts all route hop data for a given set of hops.
func (s *SQLStore) insertRouteHops(ctx context.Context, db SQLQueries,
	hops []*route.Hop, attemptID uint64) error {

	for i, hop := range hops {
		// Insert the basic route hop data and get the generated ID.
		hopID, err := db.InsertRouteHop(ctx, sqlc.InsertRouteHopParams{
			HtlcAttemptIndex: int64(attemptID),
			HopIndex:         int32(i),
			PubKey:           hop.PubKeyBytes[:],
			Scid: strconv.FormatUint(
				hop.ChannelID, 10,
			),
			OutgoingTimeLock: int32(hop.OutgoingTimeLock),
			AmtToForward:     int64(hop.AmtToForward),
			MetaData:         hop.Metadata,
		})
		if err != nil {
			return fmt.Errorf("failed to insert route hop: %w", err)
		}

		// Insert the per-hop custom records.
		if len(hop.CustomRecords) > 0 {
			for key, value := range hop.CustomRecords {
				err = db.InsertPaymentHopCustomRecord(
					ctx,
					sqlc.InsertPaymentHopCustomRecordParams{
						HopID: hopID,
						Key:   int64(key),
						Value: value,
					})
				if err != nil {
					return fmt.Errorf("failed to insert "+
						"payment hop custom record: %w",
						err)
				}
			}
		}

		// Insert MPP data if present.
		if hop.MPP != nil {
			paymentAddr := hop.MPP.PaymentAddr()
			err = db.InsertRouteHopMpp(
				ctx, sqlc.InsertRouteHopMppParams{
					HopID:       hopID,
					PaymentAddr: paymentAddr[:],
					TotalMsat:   int64(hop.MPP.TotalMsat()),
				})
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"route hop MPP: %w", err)
			}
		}

		// Insert AMP data if present.
		if hop.AMP != nil {
			rootShare := hop.AMP.RootShare()
			setID := hop.AMP.SetID()
			err = db.InsertRouteHopAmp(
				ctx, sqlc.InsertRouteHopAmpParams{
					HopID:      hopID,
					RootShare:  rootShare[:],
					SetID:      setID[:],
					ChildIndex: int32(hop.AMP.ChildIndex()),
				})
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"route hop AMP: %w", err)
			}
		}

		// Insert blinded route data if present. Every hop in the
		// blinded path must have an encrypted data record. If the
		// encrypted data is not present, we skip the insertion.
		if hop.EncryptedData == nil {
			continue
		}

		// The introduction point has a blinding point set.
		var blindingPointBytes []byte
		if hop.BlindingPoint != nil {
			blindingPointBytes = hop.BlindingPoint.
				SerializeCompressed()
		}

		// The total amount is only set for the final hop in a
		// blinded path.
		totalAmtMsat := sql.NullInt64{}
		if i == len(hops)-1 {
			totalAmtMsat = sql.NullInt64{
				Int64: int64(hop.TotalAmtMsat),
				Valid: true,
			}
		}

		err = db.InsertRouteHopBlinded(ctx,
			sqlc.InsertRouteHopBlindedParams{
				HopID:               hopID,
				EncryptedData:       hop.EncryptedData,
				BlindingPoint:       blindingPointBytes,
				BlindedPathTotalAmt: totalAmtMsat,
			},
		)
		if err != nil {
			return fmt.Errorf("failed to insert "+
				"route hop blinded: %w", err)
		}
	}

	return nil
}

// RegisterAttempt atomically records a new HTLC attempt for the specified
// payment. The attempt includes the attempt ID, session key, route information
// (hops, timelocks, amounts), and optional data such as MPP/AMP parameters,
// blinded route data, and custom records.
//
// Returns the updated MPPayment with the new attempt appended to the HTLCs
// slice, and the payment state recalculated. Returns an error if the payment
// doesn't exist or validation fails.
//
// This method is part of the PaymentControl interface, which is embedded in
// the PaymentWriter interface and ultimately the DB interface. It represents
// step 2 in the payment lifecycle control flow, called after InitPayment and
// potentially multiple times for multi-path payments.
func (s *SQLStore) RegisterAttempt(ctx context.Context,
	paymentHash lntypes.Hash, attempt *HTLCAttemptInfo) (*MPPayment,
	error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// Make sure the payment exists.
		dbPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return err
		}

		// We fetch the complete payment to determine if the payment is
		// registrable.
		//
		// TODO(ziggie): We could improve the query here since only
		// the last hop data is needed here not the complete payment
		// data.
		mpPayment, err = fetchPaymentWithCompleteData(
			ctx, s.cfg.QueryCfg, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		if err := mpPayment.Registrable(); err != nil {
			return fmt.Errorf("htlc attempt not registrable: %w",
				err)
		}

		// Verify the attempt is compatible with the existing payment.
		if err := verifyAttempt(mpPayment, attempt); err != nil {
			return fmt.Errorf("failed to verify attempt: %w", err)
		}

		// Register the plain HTLC attempt next.
		sessionKey := attempt.SessionKey()
		sessionKeyBytes := sessionKey.Serialize()

		_, err = db.InsertHtlcAttempt(ctx, sqlc.InsertHtlcAttemptParams{
			PaymentID:    dbPayment.Payment.ID,
			AttemptIndex: int64(attempt.AttemptID),
			SessionKey:   sessionKeyBytes,
			AttemptTime:  attempt.AttemptTime,
			PaymentHash:  paymentHash[:],
			FirstHopAmountMsat: int64(
				attempt.Route.FirstHopAmount.Val.Int(),
			),
			RouteTotalTimeLock: int32(attempt.Route.TotalTimeLock),
			RouteTotalAmount:   int64(attempt.Route.TotalAmount),
			RouteSourceKey:     attempt.Route.SourcePubKey[:],
		})
		if err != nil {
			return fmt.Errorf("failed to insert HTLC "+
				"attempt: %w", err)
		}

		// Insert the route level first hop custom records.
		attemptFirstHopCustomRecords := attempt.Route.
			FirstHopWireCustomRecords

		for key, value := range attemptFirstHopCustomRecords {
			//nolint:ll
			err = db.InsertPaymentAttemptFirstHopCustomRecord(
				ctx,
				sqlc.InsertPaymentAttemptFirstHopCustomRecordParams{
					HtlcAttemptIndex: int64(attempt.AttemptID),
					Key:              int64(key),
					Value:            value,
				},
			)
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"payment attempt first hop custom "+
					"record: %w", err)
			}
		}

		// Insert the route hops.
		err = s.insertRouteHops(
			ctx, db, attempt.Route.Hops, attempt.AttemptID,
		)
		if err != nil {
			return fmt.Errorf("failed to insert route hops: %w",
				err)
		}

		// We fetch the HTLC attempts again to recalculate the payment
		// state after the attempt is registered. This also makes sure
		// we have the right data in case multiple attempts are
		// registered concurrently.
		//
		// NOTE: While the caller is responsible for serializing calls
		// to RegisterAttempt per payment hash (see PaymentControl
		// interface), we still refetch here to guarantee we return
		// consistent, up-to-date data that reflects all changes made
		// within this transaction.
		mpPayment, err = fetchPaymentWithCompleteData(
			ctx, s.cfg.QueryCfg, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		return nil
	}, func() {
		mpPayment = nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to register attempt: %w", err)
	}

	return mpPayment, nil
}

// SettleAttempt marks the specified HTLC attempt as successfully settled,
// recording the payment preimage and settlement time. The preimage serves as
// cryptographic proof of payment and is atomically saved to the database.
//
// This method is part of the PaymentControl interface, which is embedded in
// the PaymentWriter interface and ultimately the DB interface. It represents
// step 3a in the payment lifecycle control flow (step 3b is FailAttempt),
// called after RegisterAttempt when an HTLC successfully completes.
func (s *SQLStore) SettleAttempt(ctx context.Context, paymentHash lntypes.Hash,
	attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		dbPayment, err := fetchPaymentByHash(ctx, db, paymentHash)
		if err != nil {
			return err
		}

		paymentStatus, err := computePaymentStatusFromDB(
			ctx, s.cfg.QueryCfg, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to compute payment "+
				"status: %w", err)
		}

		if err := paymentStatus.updatable(); err != nil {
			return fmt.Errorf("payment is not updatable: %w", err)
		}

		err = db.SettleAttempt(ctx, sqlc.SettleAttemptParams{
			AttemptIndex:   int64(attemptID),
			ResolutionTime: time.Now(),
			ResolutionType: int32(HTLCAttemptResolutionSettled),
			SettlePreimage: settleInfo.Preimage[:],
		})
		if err != nil {
			return fmt.Errorf("failed to settle attempt: %w", err)
		}

		// Fetch the complete payment after we settled the attempt.
		mpPayment, err = fetchPaymentWithCompleteData(
			ctx, s.cfg.QueryCfg, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		return nil
	}, func() {
		mpPayment = nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to settle attempt: %w", err)
	}

	return mpPayment, nil
}

// FailAttempt marks the specified HTLC attempt as failed, recording the
// failure reason, failure time, optional failure message, and the index of the
// node in the route that generated the failure. This information is atomically
// saved to the database for debugging and route optimization purposes.
//
// For single-path payments, failing the only attempt may lead to the payment
// being retried or ultimately failed via the Fail method. For multi-shard
// (MPP/AMP) payments, individual shard failures don't necessarily fail the
// entire payment; additional attempts can be registered until sufficient shards
// succeed or the payment is permanently failed.
//
// Returns the updated MPPayment with the attempt marked as failed and the
// payment state recalculated. The payment status remains StatusInFlight if
// other attempts are still in flight, or may transition based on the overall
// payment state.
//
// This method is part of the PaymentControl interface, which is embedded in
// the PaymentWriter interface and ultimately the DB interface. It represents
// step 3b in the payment lifecycle control flow (step 3a is SettleAttempt),
// called after RegisterAttempt when an HTLC fails.
func (s *SQLStore) FailAttempt(ctx context.Context, paymentHash lntypes.Hash,
	attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// Make sure the payment exists.
		dbPayment, err := fetchPaymentByHash(ctx, db, paymentHash)
		if err != nil {
			return err
		}

		paymentStatus, err := computePaymentStatusFromDB(
			ctx, s.cfg.QueryCfg, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to compute payment "+
				"status: %w", err)
		}

		// We check if the payment is updatable before failing the
		// attempt.
		if err := paymentStatus.updatable(); err != nil {
			return fmt.Errorf("payment is not updatable: %w", err)
		}

		var failureMsg bytes.Buffer
		if failInfo.Message != nil {
			err := lnwire.EncodeFailureMessage(
				&failureMsg, failInfo.Message, 0,
			)
			if err != nil {
				return fmt.Errorf("failed to encode "+
					"failure message: %w", err)
			}
		}

		err = db.FailAttempt(ctx, sqlc.FailAttemptParams{
			AttemptIndex:   int64(attemptID),
			ResolutionTime: time.Now(),
			ResolutionType: int32(HTLCAttemptResolutionFailed),
			FailureSourceIndex: sqldb.SQLInt32(
				failInfo.FailureSourceIndex,
			),
			HtlcFailReason: sqldb.SQLInt32(failInfo.Reason),
			FailureMsg:     failureMsg.Bytes(),
		})
		if err != nil {
			return fmt.Errorf("failed to fail attempt: %w", err)
		}

		mpPayment, err = fetchPaymentWithCompleteData(
			ctx, s.cfg.QueryCfg, db, dbPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		return nil
	}, func() {
		mpPayment = nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fail attempt: %w", err)
	}

	return mpPayment, nil
}

// Fail records the ultimate reason why a payment failed. This method stores
// the failure reason for record keeping but does not enforce that all HTLC
// attempts are resolved - HTLCs may still be in flight when this is called.
//
// The payment's actual status transition to StatusFailed is determined by the
// payment state calculation, which considers both the recorded failure reason
// and the current state of all HTLC attempts. The status will transition to
// StatusFailed once all HTLCs are resolved and/or a failure reason is recorded.
//
// NOTE: According to the interface contract, this should only be called when
// all active attempts are already failed. However, the implementation allows
// concurrent calls and does not validate this precondition, enabling the last
// failing attempt to record the failure reason without synchronization.
//
// This method is part of the PaymentControl interface, which is embedded in
// the PaymentWriter interface and ultimately the DB interface. It represents
// step 4 in the payment lifecycle control flow.
func (s *SQLStore) Fail(ctx context.Context, paymentHash lntypes.Hash,
	reason FailureReason) (*MPPayment, error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		result, err := db.FailPayment(ctx, sqlc.FailPaymentParams{
			PaymentIdentifier: paymentHash[:],
			FailReason:        sqldb.SQLInt32(reason),
		})
		if err != nil {
			return err
		}

		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rowsAffected == 0 {
			return ErrPaymentNotInitiated
		}

		payment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("failed to fetch payment: %w", err)
		}
		mpPayment, err = fetchPaymentWithCompleteData(
			ctx, s.cfg.QueryCfg, db, payment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		return nil
	}, func() {
		mpPayment = nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fail payment: %w", err)
	}

	return mpPayment, nil
}

// DeletePayments performs a batch deletion of payments or their failed HTLC
// attempts from the database based on the specified flags. This is a bulk
// operation that iterates through all payments and selectively deletes based
// on the criteria.
// The behavior is controlled by two flags:
//
// If failedAttemptsOnly is true, only failed HTLC attempts are deleted while
// preserving the payment records and any successful or in-flight attempts.
// The return value is always 0 when deleting attempts only.
//
// If failedAttemptsOnly is false, entire payment records are deleted including
// all associated data (HTLCs, metadata, intents). The return value is the
// number of payments deleted.
//
// The failedOnly flag further filters which payments are processed:
//   - failedOnly=true, failedAttemptsOnly=true: Delete failed attempts for
//     StatusFailed payments only
//   - failedOnly=false, failedAttemptsOnly=true: Delete failed attempts for
//     all removable payments
//   - failedOnly=true, failedAttemptsOnly=false: Delete entire payment records
//     for StatusFailed payments only
//   - failedOnly=false, failedAttemptsOnly=false: Delete all removable payment
//     records (StatusInitiated, StatusSucceeded, StatusFailed)
//
// Safety checks applied to all operations:
//   - Payments with StatusInFlight are always skipped (cannot be safely deleted
//     while HTLCs are on the network)
//   - The payment status must pass the removable() check
//
// Returns the number of complete payments deleted (0 if only deleting failed
// attempts). This is useful for cleanup operations, administrative maintenance,
// or freeing up database storage.
//
// This method is part of the PaymentWriter interface, which is embedded in
// the DB interface.
//
// TODO(ziggie): batch and use iterator instead, moreover we dont need to fetch
// the complete payment data for each payment, we can just fetch the payment ID
// and the resolution types to decide if the payment is removable.
func (s *SQLStore) DeletePayments(ctx context.Context, failedOnly,
	failedHtlcsOnly bool) (int, error) {

	var numPayments int

	extractCursor := func(row sqlc.FilterPaymentsRow) int64 {
		return row.Payment.ID
	}

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// collectFunc extracts the payment ID from each payment row.
		collectFunc := func(row sqlc.FilterPaymentsRow) (int64, error) {
			return row.Payment.ID, nil
		}

		// batchDataFunc loads only HTLC resolution types for a batch
		// of payments, which is sufficient to determine payment status.
		batchDataFunc := func(ctx context.Context, paymentIDs []int64) (
			*paymentStatusData, error) {

			return batchLoadPaymentResolutions(
				ctx, s.cfg.QueryCfg, db, paymentIDs,
			)
		}

		// processPayment processes each payment with the lightweight
		// batch-loaded resolution data.
		processPayment := func(ctx context.Context,
			dbPayment sqlc.FilterPaymentsRow,
			batchData *paymentStatusData) error {

			payment := dbPayment.Payment

			// Compute the payment status from resolution types and
			// failure reason without building the complete payment.
			resolutionTypes := batchData.resolutionTypes[payment.ID]
			status, err := computePaymentStatusFromResolutions(
				resolutionTypes, payment.FailReason,
			)
			if err != nil {
				return fmt.Errorf("failed to compute payment "+
					"status: %w", err)
			}

			// Payments which are not final yet cannot be deleted.
			// we skip them.
			if err := status.removable(); err != nil {
				return nil
			}

			// If we are only deleting failed payments, we skip
			// if the payment is not failed.
			if failedOnly && status != StatusFailed {
				return nil
			}

			// If we are only deleting failed HTLCs, we delete them
			// and return early.
			if failedHtlcsOnly {
				return db.DeleteFailedAttempts(
					ctx, payment.ID,
				)
			}

			// Otherwise we delete the payment.
			err = db.DeletePayment(ctx, payment.ID)
			if err != nil {
				return fmt.Errorf("failed to delete "+
					"payment: %w", err)
			}

			numPayments++

			return nil
		}

		queryFunc := func(ctx context.Context, lastID int64,
			limit int32) ([]sqlc.FilterPaymentsRow, error) {

			filterParams := sqlc.FilterPaymentsParams{
				NumLimit: limit,
				IndexOffsetGet: sqldb.SQLInt64(
					lastID,
				),
			}

			return db.FilterPayments(ctx, filterParams)
		}

		return sqldb.ExecuteCollectAndBatchWithSharedDataQuery(
			ctx, s.cfg.QueryCfg, int64(-1), queryFunc,
			extractCursor, collectFunc, batchDataFunc,
			processPayment,
		)
	}, func() {
		numPayments = 0
	})
	if err != nil {
		return 0, fmt.Errorf("failed to delete payments "+
			"(failedOnly: %v, failedHtlcsOnly: %v): %w",
			failedOnly, failedHtlcsOnly, err)
	}

	return numPayments, nil
}
