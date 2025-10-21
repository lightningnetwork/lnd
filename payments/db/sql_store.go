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

	FetchHtlcAttemptsForPayment(ctx context.Context, paymentID int64) ([]sqlc.FetchHtlcAttemptsForPaymentRow, error)
	FetchAllInflightAttempts(ctx context.Context) ([]sqlc.PaymentHtlcAttempt, error)
	FetchHopsForAttempt(ctx context.Context, htlcAttemptIndex int64) ([]sqlc.FetchHopsForAttemptRow, error)
	FetchHopsForAttempts(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.FetchHopsForAttemptsRow, error)

	FetchPaymentLevelFirstHopCustomRecords(ctx context.Context, paymentID int64) ([]sqlc.PaymentFirstHopCustomRecord, error)
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
func (s *SQLStore) FetchPayment(ctx context.Context,
	paymentHash lntypes.Hash) (*MPPayment, error) {

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

// FetchInFlightPayments fetches all payments with status InFlight.
//
// TODO(ziggie): Add pagination (LIMIT)) to this function?
//
// This is part of the DB interface.
func (s *SQLStore) FetchInFlightPayments(ctx context.Context) ([]*MPPayment,
	error) {

	var mpPayments []*MPPayment

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		inflightDBAttempts, err := db.FetchAllInflightAttempts(ctx)
		if err != nil {
			return fmt.Errorf("failed to fetch inflight "+
				"attempts: %w", err)
		}

		paymentIDs := make([]int64, len(inflightDBAttempts))
		for i, attempt := range inflightDBAttempts {
			paymentIDs[i] = attempt.PaymentID
		}

		dbPayments, err := db.FetchPaymentsByIDs(ctx, paymentIDs)
		if err != nil {
			return fmt.Errorf("failed to fetch payments by IDs: %w",
				err)
		}

		mpPayments = make([]*MPPayment, len(dbPayments))
		for i, dbPayment := range dbPayments {
			mpPayment, err := s.fetchPaymentWithCompleteData(
				ctx, db, dbPayment,
			)
			if err != nil {
				return fmt.Errorf("failed to fetch payment "+
					"with complete data: %w", err)
			}
			mpPayments[i] = mpPayment
		}

		return nil
	}, func() {
		mpPayments = nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch inflight "+
			"attempts: %w", err)
	}

	return mpPayments, nil
}

// DeletePayment deletes a payment from the DB given its payment hash. If
// failedHtlcsOnly is set, only failed HTLC attempts of the payment will be
// deleted.
func (s *SQLStore) DeletePayment(ctx context.Context, paymentHash lntypes.Hash,
	failedHtlcsOnly bool) error {

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		fetchPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("failed to fetch payment: %w", err)
		}
		completePayment, err := s.fetchPaymentWithCompleteData(
			ctx, db, fetchPayment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		if err := completePayment.Status.removable(); err != nil {
			return fmt.Errorf("payment %v cannot be deleted: %w",
				paymentHash, err)
		}

		// If we are only deleting failed HTLCs, we delete them.
		if failedHtlcsOnly {
			return db.DeleteFailedAttempts(
				ctx, fetchPayment.Payment.ID,
			)
		}

		// Be careful to not use s.db here, because we are in a
		// transaction, is there a way to make this more secure?
		return db.DeletePayment(ctx, fetchPayment.Payment.ID)
	}, func() {
	})
	if err != nil {
		return fmt.Errorf("failed to delete payment "+
			"(failedHtlcsOnly: %v, paymentHash: %v): %w",
			failedHtlcsOnly, paymentHash, err)
	}

	return nil
}

// DeleteFailedAttempts removes all failed HTLCs from the db. It should
// be called for a given payment whenever all inflight htlcs are
// completed, and the payment has reached a final terminal state.
func (s *SQLStore) DeleteFailedAttempts(paymentHash lntypes.Hash) error {
	// In case we are configured to keep failed payment attempts, we exit
	// early.
	if s.keepFailedPaymentAttempts {
		return nil
	}
	ctx := context.TODO()

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// We first fetch the payment to get the payment ID.
		payment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("failed to fetch payment: %w", err)
		}

		completePayment, err := s.fetchPaymentWithCompleteData(
			ctx, db, payment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with "+
				"complete data: %w", err)
		}

		if err := completePayment.Status.removable(); err != nil {
			return fmt.Errorf("payment %v cannot be deleted: %w",
				paymentHash, err)
		}

		// Then we delete the failed attempts for this payment.
		return db.DeleteFailedAttempts(ctx, payment.Payment.ID)
	}, func() {
	})
	if err != nil {
		return fmt.Errorf("failed to delete failed attempts for "+
			"payment %v: %w", paymentHash, err)
	}

	return nil
}

// InitPayment initializes a payment.
//
// This is part of the DB interface.
func (s *SQLStore) InitPayment(ctx context.Context, paymentHash lntypes.Hash,
	paymentCreationInfo *PaymentCreationInfo) error {

	// Create the payment in the database.
	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		existingPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err == nil {
			completePayment, err := s.fetchPaymentWithCompleteData(
				ctx, db, existingPayment,
			)
			if err != nil {
				return fmt.Errorf("failed to fetch payment "+
					"with complete data: %w", err)
			}

			// Check if the payment is initializable otherwise
			// we'll return early.
			err = completePayment.Status.initializable()
			if err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			// Some other error occurred
			return fmt.Errorf("failed to check existing "+
				"payment: %w", err)
		}

		// If payment exists and is failed, delete it first.
		if existingPayment.Payment.ID != 0 {
			err := db.DeletePayment(ctx, existingPayment.Payment.ID)
			if err != nil {
				return fmt.Errorf("failed to delete "+
					"payment: %w", err)
			}
		}

		var intentID *int64
		if len(paymentCreationInfo.PaymentRequest) > 0 {
			intentIDValue, err := db.InsertPaymentIntent(ctx,
				sqlc.InsertPaymentIntentParams{
					IntentType: int16(
						PaymentIntentTypeBolt11,
					),
					IntentPayload: paymentCreationInfo.
						PaymentRequest,
				})
			if err != nil {
				return fmt.Errorf("failed to initialize "+
					"payment intent: %w", err)
			}
			intentID = &intentIDValue
		}

		// Only set the intent ID if it's not nil.
		var intentIDParam sql.NullInt64
		if intentID != nil {
			intentIDParam = sqldb.SQLInt64(*intentID)
		}

		paymentID, err := db.InsertPayment(ctx,
			sqlc.InsertPaymentParams{
				IntentID: intentIDParam,
				AmountMsat: int64(
					paymentCreationInfo.Value,
				),
				CreatedAt: paymentCreationInfo.
					CreationTime.UTC(),
				PaymentIdentifier: paymentHash[:],
			})
		if err != nil {
			return fmt.Errorf("failed to insert payment: %w", err)
		}

		firstHopCustomRecords := paymentCreationInfo.
			FirstHopCustomRecords

		for key, value := range firstHopCustomRecords {
			err = db.InsertPaymentFirstHopCustomRecord(ctx,
				sqlc.InsertPaymentFirstHopCustomRecordParams{
					PaymentID: paymentID,
					Key:       int64(key),
					Value:     value,
				})
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"payment first hop custom "+
					"record: %w", err)
			}
		}

		return nil
	}, func() {
	})
	if err != nil {
		return fmt.Errorf("failed to initialize payment: %w", err)
	}

	return nil
}

// insertRouteHops inserts all route hop data for a given set of hops.
func (s *SQLStore) insertRouteHops(ctx context.Context, db SQLQueries,
	hops []*route.Hop, attemptID uint64) error {

	for i, hop := range hops {
		// Insert the basic route hop data and get the generated ID
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

		// Insert the per-hop custom records
		if len(hop.CustomRecords) > 0 {
			for key, value := range hop.CustomRecords {
				err = db.InsertPaymentHopCustomRecord(ctx,
					sqlc.InsertPaymentHopCustomRecordParams{
						HopID: hopID,
						Key:   int64(key),
						Value: value,
					},
				)
				if err != nil {
					return fmt.Errorf("failed to insert "+
						"payment hop custom "+
						"records: %w", err)
				}
			}
		}

		// Insert MPP data if present
		if hop.MPP != nil {
			paymentAddr := hop.MPP.PaymentAddr()
			err = db.InsertRouteHopMpp(ctx,
				sqlc.InsertRouteHopMppParams{
					HopID:       hopID,
					PaymentAddr: paymentAddr[:],
					TotalMsat:   int64(hop.MPP.TotalMsat()),
				},
			)
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"route hop MPP: %w", err)
			}
		}

		// Insert AMP data if present
		if hop.AMP != nil {
			rootShare := hop.AMP.RootShare()
			setID := hop.AMP.SetID()
			err = db.InsertRouteHopAmp(ctx,
				sqlc.InsertRouteHopAmpParams{
					HopID:     hopID,
					RootShare: rootShare[:],
					SetID:     setID[:],
				},
			)
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"route hop AMP: %w", err)
			}
		}

		// Insert blinded route data if present
		if hop.EncryptedData != nil || hop.BlindingPoint != nil {
			var blindingPointBytes []byte
			if hop.BlindingPoint != nil {
				blindingPointBytes = hop.BlindingPoint.
					SerializeCompressed()
			}

			err = db.InsertRouteHopBlinded(ctx,
				sqlc.InsertRouteHopBlindedParams{
					HopID:         hopID,
					EncryptedData: hop.EncryptedData,
					BlindingPoint: blindingPointBytes,
					BlindedPathTotalAmt: sqldb.SQLInt64(
						hop.TotalAmtMsat,
					),
				},
			)
			if err != nil {
				return fmt.Errorf("failed to insert "+
					"route hop blinded: %w", err)
			}
		}
	}

	return nil
}

// RegisterAttempt registers an attempt for a payment.
//
// This is part of the DB interface.
func (s *SQLStore) RegisterAttempt(ctx context.Context,
	paymentHash lntypes.Hash, attempt *HTLCAttemptInfo) (*MPPayment,
	error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// 1. First Fetch the payment and check if it is registrable.
		existingPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("failed to fetch payment: %w", err)
		}

		mpPayment, err = s.fetchPaymentWithCompleteData(
			ctx, db, existingPayment,
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

		// Fist register the plain HTLC attempt.
		// Prepare the session key.
		sessionKey := attempt.SessionKey()
		sessionKeyBytes := sessionKey.Serialize()

		_, err = db.InsertHtlcAttempt(ctx, sqlc.InsertHtlcAttemptParams{
			PaymentID:    existingPayment.Payment.ID,
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
			err = db.InsertPaymentAttemptFirstHopCustomRecord(ctx,
				//nolint:ll
				sqlc.InsertPaymentAttemptFirstHopCustomRecordParams{
					HtlcAttemptIndex: int64(attempt.AttemptID),
					Key:              int64(key),
					Value:            value,
				})
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

		// Add the attempt to the payment without fetching it from the
		// DB again.
		mpPayment.HTLCs = append(mpPayment.HTLCs, HTLCAttempt{
			HTLCAttemptInfo: *attempt,
		})

		if err := mpPayment.SetState(); err != nil {
			return fmt.Errorf("failed to set payment state: %w",
				err)
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

// SettleAttempt marks the given attempt settled with the preimage.
func (s *SQLStore) SettleAttempt(ctx context.Context, paymentHash lntypes.Hash,
	attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// Before updating the attempt, we fetch the payment to get the
		// payment ID.
		payment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("failed to fetch payment: %w", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPaymentNotInitiated
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

		mpPayment, err = s.fetchPaymentWithCompleteData(
			ctx, db, payment,
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

// FailAttempt marks the given attempt failed.
func (s *SQLStore) FailAttempt(ctx context.Context, paymentHash lntypes.Hash,
	attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// Before updating the attempt, we fetch the payment to get the
		// payment ID.
		payment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("failed to fetch payment: %w", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPaymentNotInitiated
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

		mpPayment, err = s.fetchPaymentWithCompleteData(
			ctx, db, payment,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch payment with"+
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

// Fail transitions a payment into the Failed state, and records the ultimate
// reason the payment failed. Note that this should only be called when all
// active attempts are already failed. After invoking this method, InitPayment
// should return nil on its next call for this payment hash, allowing the user
// to make a subsequent payments for the same payment hash.
func (s *SQLStore) Fail(ctx context.Context, paymentHash lntypes.Hash,
	reason FailureReason) (*MPPayment, error) {

	var mpPayment *MPPayment

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		result, err := db.FailPayment(ctx, sqlc.FailPaymentParams{
			PaymentIdentifier: paymentHash[:],
			FailReason:        sqldb.SQLInt32(reason),
		})
		if err != nil {
			return fmt.Errorf("failed to fail payment: %w", err)
		}

		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected: %w",
				err)
		}
		if rowsAffected == 0 {
			return ErrPaymentNotInitiated
		}

		payment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("failed to fetch payment: %w", err)
		}
		mpPayment, err = s.fetchPaymentWithCompleteData(
			ctx, db, payment,
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

// DeletePayments deletes all payments from the DB given the specified flags.
//
// TODO(ziggie): batch and use iterator instead.
func (s *SQLStore) DeletePayments(ctx context.Context, failedOnly,
	failedHtlcsOnly bool) (int, error) {

	var numPayments int

	extractCursor := func(
		row sqlc.FilterPaymentsRow) int64 {

		return row.Payment.ID
	}

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
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

			// Payments which are not final yet cannot be deleted.
			// we skip them.
			if err := mpPayment.Status.removable(); err != nil {
				return nil
			}

			// If we are only deleting failed payments, we skip
			// if the payment is not failed.
			if failedOnly && mpPayment.Status != StatusFailed {
				return nil
			}

			// If we are only deleting failed HTLCs, we delete them
			// and return early.
			if failedHtlcsOnly {
				return db.DeleteFailedAttempts(
					ctx, dbPayment.Payment.ID,
				)
			}

			// Otherwise we delete the payment.
			err = db.DeletePayment(ctx, dbPayment.Payment.ID)
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
				// For now there are only BOLT 11 payment
				// intents.
				IntentType: sqldb.SQLInt16(
					PaymentIntentTypeBolt11,
				),
				IndexOffsetGet: sqldb.SQLInt64(
					lastID,
				),
			}

			return db.FilterPayments(ctx, filterParams)
		}

		return sqldb.ExecutePaginatedQuery(
			ctx, s.cfg.QueryCfg, int64(-1), queryFunc,
			extractCursor, processPayment,
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
