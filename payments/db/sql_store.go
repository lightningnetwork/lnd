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

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

const defaultQueryPaginationLimit = 100

// SQLQueries is a subset of the sqlc.Querier interface that can be used to
// execute queries against the SQL payments tables.
//
//nolint:ll,interfacebloat
type SQLQueries interface {
	// Sequence operations.
	NextHtlcAttemptIndex(ctx context.Context) (int64, error)

	/*
		Payment DB read operations.
	*/
	FilterPayments(ctx context.Context, arg sqlc.FilterPaymentsParams) ([]sqlc.Payment, error)
	FetchHtlcAttempts(ctx context.Context, arg sqlc.FetchHtlcAttemptsParams) ([]sqlc.PaymentHtlcAttempt, error)
	FetchHopsForAttempt(ctx context.Context, htlcAttemptIndex int64) ([]sqlc.PaymentRouteHop, error)
	FetchCustomRecordsForHop(ctx context.Context, hopID int64) ([]sqlc.PaymentRouteHopCustomRecord, error)
	FetchFirstHopCustomRecords(ctx context.Context, paymentID int64) ([]sqlc.PaymentFirstHopCustomRecord, error)

	// Fetch single payment without attempt data.
	FetchPayment(ctx context.Context, paymentHash []byte) (sqlc.Payment, error)

	// Fetch inflight HTLC attempts.
	FetchInflightHTLCAttempts(ctx context.Context) ([]sqlc.PaymentHtlcAttempt, error)

	/*
		Payment DB write operations.
	*/

	// Delete payment related data.
	DeleteFailedAttempts(ctx context.Context, paymentID int64) error
	DeletePayment(ctx context.Context, paymentHash []byte) error
	DeletePaymentByID(ctx context.Context, paymentID int64) error
	DeleteHTLCAttempt(ctx context.Context, attemptID int64) error

	// Insert payment related data.
	InsertPayment(ctx context.Context, arg sqlc.InsertPaymentParams) (int64, error)
	InsertFirstHopCustomRecord(ctx context.Context, arg sqlc.InsertFirstHopCustomRecordParams) error
	InsertHtlcAttempt(ctx context.Context, arg sqlc.InsertHtlcAttemptParams) (int64, error)
	InsertHop(ctx context.Context, arg sqlc.InsertHopParams) (int64, error)
	InsertHopCustomRecord(ctx context.Context, arg sqlc.InsertHopCustomRecordParams) error

	// Update HTLC attempt operations.
	UpdateHtlcAttemptSettleInfo(ctx context.Context, arg sqlc.UpdateHtlcAttemptSettleInfoParams) (int64, error)
	UpdateHtlcAttemptFailInfo(ctx context.Context, arg sqlc.UpdateHtlcAttemptFailInfoParams) (int64, error)

	// Update payment operations.
	UpdatePaymentFailReason(ctx context.Context, arg sqlc.UpdatePaymentFailReasonParams) (int64, error)

	/*
		Duplicate payment DB read operations.
	*/
	FetchDuplicatePayments(ctx context.Context, paymentHash []byte) ([]sqlc.DuplicatePayment, error)
	FetchDuplicateHtlcAttempts(ctx context.Context, arg sqlc.FetchDuplicateHtlcAttemptsParams) ([]sqlc.DuplicatePaymentHtlcAttempt, error)
	FetchDuplicateHopsForAttempt(ctx context.Context, htlcAttemptIndex int64) ([]sqlc.DuplicatePaymentRouteHop, error)
	FetchDuplicateHopCustomRecordsForHop(ctx context.Context, hopID int64) ([]sqlc.DuplicatePaymentRouteHopCustomRecord, error)

	/*
		Duplicate payment DB write operations.
		These operations are only needed for the migration of the
		kvstore because the current version does not allow to create
		payments with the same payment hash.

		Duplicate payments are legacy payments and are deleted as soon
		as their corresponding payment with the same payment hash in the
		main payments table is is deleted.
	*/
	InsertDuplicatePayment(ctx context.Context, arg sqlc.InsertDuplicatePaymentParams) (int64, error)
	InsertDuplicateHtlcAttempt(ctx context.Context, arg sqlc.InsertDuplicateHtlcAttemptParams) (int64, error)
	InsertDuplicateHop(ctx context.Context, arg sqlc.InsertDuplicateHopParams) (int64, error)
	InsertDuplicateHopCustomRecord(ctx context.Context, arg sqlc.InsertDuplicateHopCustomRecordParams) error
}

var _ PaymentDB = (*SQLStore)(nil)

// SQLPaymentsQueriesTxOptions defines the set of db txn options the
// SQLInvoiceQueries understands.
type SQLPaymentsQueriesTxOptions struct {
	// readOnly governs if a read only transaction is needed or not.
	readOnly bool
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This implements the TxOptions.
func (a *SQLPaymentsQueriesTxOptions) ReadOnly() bool {
	return a.readOnly
}

// NewSQLPaymentsQueryReadTx creates a new read transaction option set.
func NewSQLPaymentsQueryReadTx() SQLPaymentsQueriesTxOptions {
	return SQLPaymentsQueriesTxOptions{
		readOnly: true,
	}
}

// BatchedSQLPaymentsQueries is a version of the SQLQueries that's capable
// of batched database operations.
type BatchedSQLPaymentsQueries interface {
	SQLQueries

	sqldb.BatchedTx[SQLQueries]
}

// SQLStore represents a storage backend.
type SQLStore struct {
	db          BatchedSQLPaymentsQueries
	clock       clock.Clock
	storeConfig *StoreOptions
}

// defaultSQLStoreOptions returns the default options for the SQL store.
func defaultSQLStoreOptions() *StoreOptions {
	return &StoreOptions{
		paginationLimit: defaultQueryPaginationLimit,
	}
}

// NewSQLStore creates a new SQLStore instance given a open
// BatchedSQLPaymentsQueries storage backend.
func NewSQLStore(db BatchedSQLPaymentsQueries,
	clock clock.Clock, options ...OptionModifier) *SQLStore {

	opts := defaultSQLStoreOptions()
	for _, applyOption := range options {
		applyOption(opts)
	}

	return &SQLStore{
		db:          db,
		clock:       clock,
		storeConfig: opts,
	}
}

// QueryPayments queries the payments from the database.
func (s *SQLStore) QueryPayments(ctx context.Context, query Query) (Response,
	error) {

	if query.MaxPayments == 0 {
		return Response{}, fmt.Errorf("max payments must be non-zero")
	}

	var (
		allPayments          []*MPPayment
		allDuplicatePayments = make(map[lntypes.Hash][]*MPPayment)
		readTxOpt            = NewSQLPaymentsQueryReadTx()
	)

	err := s.db.ExecTx(ctx, &readTxOpt, func(db SQLQueries) error {
		return queryWithLimit(func(offset int) (int, error) {
			filterParams := sqlc.FilterPaymentsParams{
				NumLimit:  int32(s.storeConfig.paginationLimit),
				NumOffset: int32(offset),
				Reverse:   query.Reversed,
			}

			if query.Reversed {
				if query.IndexOffset == 0 {
					indexLet := sqldb.SQLInt64(
						int64(math.MaxInt64),
					)
					filterParams.IndexOffsetLet = indexLet
				} else {
					indexGet := sqldb.SQLInt64(
						int64(query.IndexOffset - 1),
					)
					filterParams.IndexOffsetLet = indexGet
				}
			} else {
				filterParams.IndexOffsetGet = sqldb.SQLInt64(
					int64(query.IndexOffset + 1),
				)
			}

			// Add date filters if specified.
			if query.CreationDateStart != 0 {
				filterParams.CreatedAfter = sqldb.SQLTime(
					time.Unix(query.CreationDateStart, 0).
						UTC(),
				)
			}
			if query.CreationDateEnd != 0 {
				filterParams.CreatedBefore = sqldb.SQLTime(
					time.Unix(query.CreationDateEnd+1, 0).
						UTC(),
				)
			}

			// If we are only interested in settled payments (no
			// failed or in-flight payments), we can exclude failed
			// the failed one here with the query params. We will
			// still fetch INFLIGHT payments which we then will
			// drop in the code below. There is no easy way
			// currently to also not fetch INFLIGHT payments.
			if !query.IncludeIncomplete {
				filterParams.ExcludeFailed = sqldb.SQLBool(true)
			}

			// We fetch the payment with a limit and offset.
			dbPayments, err := db.FilterPayments(ctx, filterParams)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("unable to fetch "+
					"payments: %w", err)
			}

			// Fetch all the necessary data which belongs to the
			// payment.
			for _, dbPayment := range dbPayments {
				payment, err := s.fetchPaymentWithCompleteData(
					ctx, db, dbPayment,
				)
				if err != nil {
					return 0, fmt.Errorf("unable to fetch "+
						"complete payment data: %w",
						err)
				}

				if payment.Status != StatusSucceeded &&
					!query.IncludeIncomplete {
					continue
				}

				allPayments = append(allPayments, payment)

				// We also fetch potential duplicate payments.
				if dbPayment.HasDuplicatePayment {
					err := s.fetchDuplicatePaymentsForPayment(ctx, db, dbPayment, allDuplicatePayments)
					if err != nil {
						return 0, fmt.Errorf("unable to fetch duplicate payments: %w", err)
					}
				}

				// Check if we've reached the maximum number of
				// payments and break out of the loop in case
				// we've reached the limit.
				if uint64(len(allPayments)) >=
					query.MaxPayments {

					break
				}
			}

			return len(dbPayments), nil
		}, s.storeConfig.paginationLimit)
	}, func() {
		allPayments = nil
		allDuplicatePayments = nil
	})

	if err != nil {
		return Response{}, fmt.Errorf("unable to query payments: %w", err)
	}

	// Handle case where no payments were found
	if len(allPayments) == 0 {
		return Response{
			Payments:          allPayments,
			DuplicatePayments: allDuplicatePayments,
			FirstIndexOffset:  0,
			LastIndexOffset:   0,
			TotalCount:        0,
		}, nil
	}

	return Response{
		Payments:         allPayments,
		FirstIndexOffset: uint64(allPayments[0].SequenceNum),
		LastIndexOffset:  uint64(allPayments[len(allPayments)-1].SequenceNum),
		TotalCount:       uint64(len(allPayments)),
	}, nil
}

// DeletePayment deletes a payment from the database using the payment hash
// as a reference.
func (s *SQLStore) DeletePayment(paymentHash lntypes.Hash,
	failedAttemptsOnly bool) error {

	ctx := context.TODO()
	var writeTxOpts SQLPaymentsQueriesTxOptions

	err := s.db.ExecTx(ctx, &writeTxOpts, func(db SQLQueries) error {
		payment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("payment not found: %w",
					ErrPaymentNotInitiated)
			}
			return fmt.Errorf("unable to fetch payment: %w", err)
		}

		completePayment, err := s.fetchPaymentWithCompleteData(
			ctx, db, payment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch complete "+
				"payment data: %w", err)
		}
		if err := completePayment.Status.Removable(); err != nil {
			return fmt.Errorf("payment is still in flight: %w", err)
		}

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

// DeletePayments deletes payments from the database.
//
// TODO(ziggie): Consider using a max payments parameter to limit the number of
// payments to delete in one go.
func (s *SQLStore) DeletePayments(failedOnly, failedAttemptsOnly bool) (int,
	error) {

	ctx := context.TODO()
	var writeTxOpts SQLPaymentsQueriesTxOptions

	var (
		paymentsToDelete []*MPPayment
	)

	err := s.db.ExecTx(ctx, &writeTxOpts, func(db SQLQueries) error {
		err := queryWithLimit(func(offset int) (int, error) {
			filterParams := sqlc.FilterPaymentsParams{
				// Currently there is no pagination we fetch
				// all payments at once.
				IndexOffsetGet: 1,
				NumLimit: int32(
					s.storeConfig.paginationLimit,
				),
				NumOffset: int32(offset),
				Reverse:   false,
			}

			// We fetch the payment with a limit and offset.
			//
			// NOTE: This can be expensive without proper
			// pagination and fetching all payments to delete at
			// once.
			sqlPayments, err := db.FilterPayments(ctx, filterParams)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("unable to fetch "+
					"payments: %w", err)
			}

			// Fetch all the necessary data which belongs to the
			// payment so we can decide whether we can delete it
			// or not and potentially need to wait for pending
			// HTLCs to resolve.
			for _, sqlPayment := range sqlPayments {
				payment, err := s.fetchPaymentWithCompleteData(
					ctx, db, sqlPayment,
				)
				if err != nil {
					return 0, fmt.Errorf("unable to fetch "+
						"complete payment data: %w",
						err)
				}

				err = payment.Status.Removable()
				if err != nil {
					log.Tracef("Skipping payment %d"+
						"(hash=%s) with status %s "+
						"because it is not removable",
						payment.SequenceNum,
						payment.Info.PaymentIdentifier,
						payment.Status,
					)

					continue
				}

				// If we are only deleting failed payments, we
				// can return if this one is not.
				if failedOnly &&
					payment.Status != StatusFailed {
					// If we are only deleting failed HTLCs,
					// we can return if this one is not.
					log.Tracef("Skipping payment %d"+
						"(hash=%s) with status %s "+
						"because it is not failed",
						payment.SequenceNum,
						payment.Info.PaymentIdentifier,
						payment.Status)

					continue
				}

				// In case only failed attempts should be
				// deleted, we will not delete any payments.
				if failedAttemptsOnly {
					err := db.DeleteFailedAttempts(
						ctx, int64(payment.SequenceNum),
					)

					if err != nil {
						return 0, fmt.Errorf("unable "+
							"to delete failed "+
							"attempts for "+
							"payment %d (hash=%s): %w",
							payment.SequenceNum,
							payment.Info.PaymentIdentifier,
							err,
						)
					}

					continue
				}

				// We can remove this payment because it is
				// either settled or failed.
				paymentsToDelete = append(
					paymentsToDelete, payment,
				)
			}

			return len(sqlPayments), nil
		}, s.storeConfig.paginationLimit)
		if err != nil {
			return fmt.Errorf("unable to fetch payments: %w", err)
		}

		// We now delete the payments.
		for _, payment := range paymentsToDelete {
			err := db.DeletePaymentByID(
				ctx, int64(payment.SequenceNum),
			)
			if err != nil {
				return fmt.Errorf("unable to delete "+
					"payment: %w", err)
			}
		}

		return nil
	}, func() {
		paymentsToDelete = nil
	})

	if err != nil {
		return 0, fmt.Errorf("unable to delete payments: %w", err)
	}

	return len(paymentsToDelete), nil
}

// DeleteFailedAttempts deletes all failed attempts for a payment. We use the
// delete function here which includes the check wether the payment is still
// INFLIGHT because for INFLIGHT payments we don't delete any data.
func (s *SQLStore) DeleteFailedAttempts(paymentHash lntypes.Hash) error {
	if !s.storeConfig.keepFailedPaymentAttempts {
		const failedAttemptsOnly = true
		return s.DeletePayment(paymentHash, failedAttemptsOnly)
	}

	// If we are configured to keep failed payment attempts, we
	// don't delete any data.
	return nil
}

// InitPayment initializes a payment in the database.
func (s *SQLStore) InitPayment(paymentHash lntypes.Hash,
	paymentCreationInfo *PaymentCreationInfo) error {

	var (
		ctx         = context.TODO()
		writeTxOpts SQLPaymentsQueriesTxOptions
	)

	err := s.db.ExecTx(ctx, &writeTxOpts, func(db SQLQueries) error {
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
					paymentCreationInfo,
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
		if err := payment.Status.Initializable(); err != nil {
			return fmt.Errorf("payment is not "+
				"initializable: %w", err)
		}

		// This should never happen, but we check it for
		// completeness.
		if err := payment.Status.Removable(); err != nil {
			return fmt.Errorf("payment is not "+
				"removable: %w", err)
		}

		// We delete the payment to avoid duplicate payments.
		err = db.DeletePayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("unable to delete "+
				"payment: %w", err)
		}

		// And insert a new payment.
		err = s.insertNewPayment(
			ctx, db, paymentHash, paymentCreationInfo,
		)
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
			customRecordParams := sqlc.InsertFirstHopCustomRecordParams{
				PaymentID: paymentID,
				Key:       int64(key),
				Value:     value,
			}

			err := db.InsertFirstHopCustomRecord(
				ctx, customRecordParams,
			)
			if err != nil {
				return fmt.Errorf("unable to insert "+
					"first hop custom record: %w", err)
			}
		}
	}

	return nil
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
		RouteTotalTimelock: int32(attempt.Route.TotalTimeLock),
		RouteTotalAmount:   int64(attempt.Route.TotalAmount),
		RouteSourceKey:     routeSourceKey[:],
	})
	if err != nil {
		return fmt.Errorf("unable to insert htlc attempt: %w", err)
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

			err = s.insertHopCustomRecords(
				ctx, db, hopID, hop.CustomRecords,
			)
			if err != nil {
				return fmt.Errorf("unable to insert hop "+
					"custom records: %w", err)
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
		ChanID:           strconv.FormatUint(hop.ChannelID, 10),
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

// insertHopCustomRecords inserts custom records for a hop.
func (s *SQLStore) insertHopCustomRecords(ctx context.Context, db SQLQueries,
	hopID int64, customRecords record.CustomSet) error {

	for key, value := range customRecords {
		err := db.InsertHopCustomRecord(ctx, sqlc.InsertHopCustomRecordParams{
			HopID: hopID,
			Key:   int64(key),
			Value: value,
		})
		if err != nil {
			return fmt.Errorf("unable to insert hop custom "+
				"record: %w", err)
		}
	}

	return nil
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

// RegisterAttempt registers a new attempt for a payment.
func (s *SQLStore) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *HTLCAttemptInfo) (*MPPayment, error) {

	var (
		ctx         = context.TODO()
		writeTxOpts SQLPaymentsQueriesTxOptions
		mppPayment  *MPPayment
	)

	err := s.db.ExecTx(ctx, &writeTxOpts, func(db SQLQueries) error {
		dbPayment, err := db.FetchPayment(ctx, paymentHash[:])
		if err != nil {
			return fmt.Errorf("unable to fetch payment: %w", err)
		}

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
		// the payment again.
		payment.HTLCs = append(payment.HTLCs, HTLCAttempt{
			HTLCAttemptInfo: *attempt,
		})

		// We update the payment because the state is now different and
		// we want to return the updated payment.
		payment.setState()

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

// SettleAttempt settles an attempt for a payment.
func (s *SQLStore) SettleAttempt(paymentHash lntypes.Hash, attemptID uint64,
	settleInfo *HTLCSettleInfo) (*MPPayment, error) {

	var (
		ctx         = context.TODO()
		writeTxOpts SQLPaymentsQueriesTxOptions
		mppPayment  *MPPayment
	)

	err := s.db.ExecTx(ctx, &writeTxOpts, func(db SQLQueries) error {
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

		if err := payment.Status.Updatable(); err != nil {
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

// FailAttempt fails an attempt for a payment.
func (s *SQLStore) FailAttempt(paymentHash lntypes.Hash, attemptID uint64,
	failInfo *HTLCFailInfo) (*MPPayment, error) {

	var (
		ctx         = context.TODO()
		writeTxOpts SQLPaymentsQueriesTxOptions
		mppPayment  *MPPayment
	)

	err := s.db.ExecTx(ctx, &writeTxOpts, func(db SQLQueries) error {
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

		if err := payment.Status.Updatable(); err != nil {
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

// FetchPayment fetches a single payment from the database.
func (s *SQLStore) FetchPayment(paymentHash lntypes.Hash) (*MPPayment, error) {
	var (
		ctx        = context.TODO()
		readTxOpts = NewSQLPaymentsQueryReadTx()
		mppPayment *MPPayment
	)

	err := s.db.ExecTx(ctx, &readTxOpts, func(db SQLQueries) error {
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
	}, func() {})

	if err != nil {
		return nil, fmt.Errorf("unable to fetch payment: %w", err)
	}

	return mppPayment, nil
}

// FetchInFlightPayments fetches all payments which are INFLIGHT.
func (s *SQLStore) FetchInFlightPayments() ([]*MPPayment, error) {
	var (
		ctx              = context.TODO()
		readTxOpts       = NewSQLPaymentsQueryReadTx()
		inFlightPayments map[lntypes.Hash]*MPPayment
	)

	err := s.db.ExecTx(ctx, &readTxOpts, func(db SQLQueries) error {
		dbInflightAttempts, err := db.FetchInflightHTLCAttempts(ctx)
		if err != nil {
			return fmt.Errorf("unable to fetch inflight htlc "+
				"attempts: %w", err)
		}

		// Based on the inflight attempts we fetch the payment and all
		// its data. We can have several attempts for the same payment
		// therefore we use a map to deduplicate the payments.
		for _, dbAttempt := range dbInflightAttempts {
			dbPayment, err := db.FetchPayment(
				ctx, dbAttempt.PaymentHash[:],
			)
			if err != nil {
				return fmt.Errorf("unable to fetch payment: %w",
					err)
			}

			mppPayment, err := s.fetchPaymentWithCompleteData(
				ctx, db, dbPayment,
			)
			if err != nil {
				return fmt.Errorf("unable to fetch payment: %w",
					err)
			}

			inFlightPayments[lntypes.Hash(dbPayment.PaymentHash)] = mppPayment
		}

		return nil
	}, func() {
		inFlightPayments = nil
	})

	if err != nil {
		return nil, fmt.Errorf("unable to fetch in-flight "+
			"payments: %w", err)
	}

	// Convert the map to a slice.
	inFlightPaymentsSlice := make([]*MPPayment, 0, len(inFlightPayments))
	for _, mppPayment := range inFlightPayments {
		inFlightPaymentsSlice = append(
			inFlightPaymentsSlice, mppPayment,
		)
	}

	return inFlightPaymentsSlice, nil
}

// Fail fails a payment and returns the updated MPPayment.
func (s *SQLStore) Fail(paymentHash lntypes.Hash,
	reason FailureReason) (*MPPayment, error) {

	var (
		ctx         = context.TODO()
		writeTxOpts SQLPaymentsQueriesTxOptions
		mppPayment  *MPPayment
	)

	err := s.db.ExecTx(ctx, &writeTxOpts, func(db SQLQueries) error {
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

		_, err = db.UpdatePaymentFailReason(ctx,
			sqlc.UpdatePaymentFailReasonParams{
				PaymentHash: paymentHash[:],
				FailReason: sql.NullInt32{
					Int32: int32(reason),
					Valid: true,
				},
			})
		if err != nil {
			return fmt.Errorf("unable to update payment fail "+
				"reason: %w", err)
		}

		// Instead of another round trip to fetch the payment, we just
		// update the payment here as well.

		payment.FailureReason = &reason

		// We update also the payment state and status.
		payment.setState()

		mppPayment = payment

		return nil
	}, func() {
		mppPayment = nil
	})

	if err != nil {
		return nil, fmt.Errorf("unable to fail payment: %w", err)
	}

	return mppPayment, nil
}

// NextID gets the next HTLC attempt ID from the database sequence.
// This function always increments the sequence when called, regardless of
// whether an HTLC attempt is actually inserted.
func (s *SQLStore) NextID() (uint64, error) {
	var (
		ctx         = context.TODO()
		writeTxOpts SQLPaymentsQueriesTxOptions
		nextID      int64
	)

	err := s.db.ExecTx(ctx, &writeTxOpts, func(db SQLQueries) error {
		dbNextID, err := db.NextHtlcAttemptIndex(ctx)
		if err != nil {
			return fmt.Errorf("unable to update next HTLC attempt "+
				"index: %w", err)
		}

		nextID = dbNextID

		return nil
	}, func() {
		nextID = 0
	})

	if err != nil {
		return 0, fmt.Errorf("unable to update next HTLC attempt "+
			"index: %w", err)
	}

	return uint64(nextID), nil
}

// queryWithLimit is a helper method that can be used to query the database
// using a limit and offset. The passed query function should return the
// number of rows returned and an error if any.
func queryWithLimit(query func(int) (int, error), limit int) error {
	offset := 0
	for {
		rows, err := query(offset)
		if err != nil {
			return err
		}

		if rows < limit {
			return nil
		}

		offset += limit
	}
}

// fetchPaymentWithCompleteData fetches a payment and all its associated data
// (HTLC attempts, hops, custom records) to create a complete internal
// representation of the payment.
//
// NOTE: This does also add the payment status and payment state which is
// derived from the db data.
func (s *SQLStore) fetchPaymentWithCompleteData(ctx context.Context,
	db SQLQueries, sqlPayment sqlc.Payment) (*MPPayment, error) {

	var htlcAttempts []HTLCAttempt

	dbHtlcAttempts, err := db.FetchHtlcAttempts(
		ctx, sqlc.FetchHtlcAttemptsParams{
			PaymentID: sqlPayment.ID,
		},
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unable to fetch htlc attempts for "+
			"payment(id=%d): %w", sqlPayment.ID, err)
	}

	// Convert the db htlc attempts to our internal htlc attempts data
	// structure.
	for _, dbAttempt := range dbHtlcAttempts {
		dbHops, err := db.FetchHopsForAttempt(
			ctx, dbAttempt.AttemptIndex,
		)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("unable to fetch hops for htlc "+
				"attempt(id=%d): %w", dbAttempt.AttemptIndex,
				err)
		}
		attempt, err := unmarshalHtlcAttempt(dbAttempt, dbHops)
		if err != nil {
			return nil, fmt.Errorf("unable to convert htlc "+
				"attempt(id=%d): %w", dbAttempt.AttemptIndex,
				err)
		}

		hops := attempt.Route.Hops

		// We also attach the custom records to the hop which are in a
		// separate table.
		for i, dbHop := range dbHops {
			dbHopCustomRecords, err := db.FetchCustomRecordsForHop(
				ctx, dbHop.ID,
			)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("unable to fetch "+
					"custom records for hop(id=%d): %w",
					dbHop.ID, err)
			}

			hops[i].CustomRecords, err = unmarshalHopCustomRecords(
				dbHopCustomRecords,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to unmarshal "+
					"hop custom records: %w", err)
			}
		}

		htlcAttempts = append(htlcAttempts, *attempt)
	}

	dbFirstHopCustomRecords, err := db.FetchFirstHopCustomRecords(
		ctx, sqlPayment.ID,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unable to fetch first hop "+
			"custom records for payment(id=%d): %w",
			sqlPayment.ID, err)
	}

	firstHopCustomRecords, err := unmarshalFirstHopCustomRecords(
		dbFirstHopCustomRecords,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal first hop "+
			"custom records: %w", err)
	}

	// Convert payment hash from bytes to lntypes.Hash
	var paymentHash lntypes.Hash
	copy(paymentHash[:], sqlPayment.PaymentHash)

	// Create PaymentCreationInfo from the payment data
	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentHash,
		Value: lnwire.MilliSatoshi(
			sqlPayment.AmountMsat,
		),
		CreationTime:          sqlPayment.CreatedAt.Local(),
		PaymentRequest:        sqlPayment.PaymentRequest,
		FirstHopCustomRecords: firstHopCustomRecords,
	}

	var failureReason *FailureReason
	if sqlPayment.FailReason.Valid {
		reason := FailureReason(sqlPayment.FailReason.Int32)
		failureReason = &reason
	}

	// Create MPPayment object
	payment := &MPPayment{
		SequenceNum:   uint64(sqlPayment.ID),
		FailureReason: failureReason,
		Info:          creationInfo,
		HTLCs:         htlcAttempts,
	}

	// We get the state of the payment in case it has inflight HTLCs.
	//
	// TODO(ziggie): Refactor this code so we can determine the payment
	// state without having to fetch all HTLCs.
	payment.setState()

	return payment, nil
}

// fetchDuplicatePaymentsForPayment fetches all duplicate payments for a given
// payment and adds them to the provided map.
func (s *SQLStore) fetchDuplicatePaymentsForPayment(ctx context.Context,
	db SQLQueries, dbPayment sqlc.Payment,
	allDuplicatePayments map[lntypes.Hash][]*MPPayment) error {

	duplicatePayments, err := db.FetchDuplicatePayments(
		ctx, dbPayment.PaymentHash[:],
	)
	if err != nil {
		return fmt.Errorf("unable to fetch duplicate payments: %w", err)
	}

	for _, duplicatePayment := range duplicatePayments {
		duplicatePayment, err := s.fetchDuplicatePaymentWithCompleteData(
			ctx, db, duplicatePayment,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch duplicate "+
				"payment: %w", err)
		}

		paymentHash := lntypes.Hash(dbPayment.PaymentHash)
		allDuplicatePayments[paymentHash] = append(
			allDuplicatePayments[paymentHash], duplicatePayment,
		)
	}

	return nil
}

// fetchDuplicatePaymentWithCompleteData fetches a duplicate payment and all
// its associated data (HTLC attempts, hops, custom records) to create a
// complete internal representation of the payment.
//
// NOTE: This does also add the payment status and payment state which is
// derived from the other data via the `setState` method.
func (s *SQLStore) fetchDuplicatePaymentWithCompleteData(ctx context.Context,
	db SQLQueries, dbPayment sqlc.DuplicatePayment) (*MPPayment, error) {

	var htlcAttempts []HTLCAttempt

	dbHtlcAttempts, err := db.FetchDuplicateHtlcAttempts(
		ctx, sqlc.FetchDuplicateHtlcAttemptsParams{
			DuplicatePaymentID: dbPayment.ID,
		},
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unable to fetch htlc attempts for "+
			"payment(id=%d): %w", dbPayment.ID, err)
	}

	// Convert the db htlc attempts to our internal htlc attempts data
	// structure.
	for _, dbAttempt := range dbHtlcAttempts {
		dbHops, err := db.FetchDuplicateHopsForAttempt(
			ctx, dbAttempt.AttemptIndex,
		)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("unable to fetch hops for htlc "+
				"attempt(id=%d): %w", dbAttempt.AttemptIndex,
				err)
		}
		attempt, err := unmarshalDuplicateHtlcAttempt(dbAttempt, dbHops)
		if err != nil {
			return nil, fmt.Errorf("unable to convert htlc "+
				"attempt(id=%d): %w", dbAttempt.AttemptIndex,
				err)
		}

		hops := attempt.Route.Hops

		// We also attach the custom records to the hop which are in a
		// separate table.
		for i, dbHop := range dbHops {
			dbHopCustomRecords, err := db.FetchDuplicateHopCustomRecordsForHop(
				ctx, dbHop.ID,
			)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("unable to fetch "+
					"custom records for hop(id=%d): %w",
					dbHop.ID, err)
			}

			hops[i].CustomRecords, err = unmarshalDuplicateHopCustomRecords(
				dbHopCustomRecords,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to unmarshal "+
					"hop custom records: %w", err)
			}
		}

		htlcAttempts = append(htlcAttempts, *attempt)
	}

	// Convert payment hash from bytes to lntypes.Hash
	var paymentHash lntypes.Hash
	copy(paymentHash[:], dbPayment.PaymentHash)

	// Create PaymentCreationInfo from the payment data
	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentHash,
		Value: lnwire.MilliSatoshi(
			dbPayment.AmountMsat,
		),
		CreationTime:   dbPayment.CreatedAt.Local(),
		PaymentRequest: dbPayment.PaymentRequest,
	}

	var failureReason *FailureReason
	if dbPayment.FailReason.Valid {
		reason := FailureReason(dbPayment.FailReason.Int32)
		failureReason = &reason
	}

	// Create MPPayment object
	payment := &MPPayment{
		SequenceNum:   uint64(dbPayment.ID),
		FailureReason: failureReason,
		Info:          creationInfo,
		HTLCs:         htlcAttempts,
	}

	// We update the status of the payment now.
	payment.setState()

	return payment, nil
}
