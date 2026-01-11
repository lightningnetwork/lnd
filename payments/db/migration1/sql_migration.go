package migration1

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/payments/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/routing/route"
)

// MigrationStats tracks migration progress.
type MigrationStats struct {
	TotalPayments      int64
	SuccessfulPayments int64
	FailedPayments     int64
	InFlightPayments   int64
	TotalAttempts      int64
	SettledAttempts    int64
	FailedAttempts     int64
	InFlightAttempts   int64
	TotalHops          int64
	DuplicatePayments  int64
	DuplicateEntries   int64
	MigrationDuration  time.Duration
}

// MigratePaymentsKVToSQL migrates payments from KV to SQL in a single
// transaction and validates migrated data in batches. This function is called
// by the migration framework which provides the transaction.
func MigratePaymentsKVToSQL(ctx context.Context, kvBackend kvdb.Backend,
	sqlDB SQLQueries, cfg *SQLStoreConfig) error {

	if cfg == nil || cfg.QueryCfg == nil {
		return fmt.Errorf("missing SQL store config for validation")
	}

	if cfg.QueryCfg.MaxBatchSize == 0 {
		return fmt.Errorf("invalid max batch size for validation")
	}

	stats := &MigrationStats{}
	startTime := time.Now()

	log.Infof("Starting payment migration from KV to SQL...")

	lastReport := time.Now()
	var validationBatch []migratedPaymentRef
	var validatedPayments int64

	// Open the KV backend in read-only mode.
	err := kvBackend.View(func(kvTx kvdb.RTx) error {
		// In case we start with an empty database, there are no
		// payments to migrate.
		paymentsBucket := kvTx.ReadBucket(paymentsRootBucket)
		if paymentsBucket == nil {
			log.Infof("No payments bucket found - database is " +
				"empty")

			return nil
		}

		// The index bucket maps sequence number -> payment hash.
		indexes := kvTx.ReadBucket(paymentsIndexBucket)
		if indexes == nil {
			return fmt.Errorf("index bucket does not exist")
		}

		// We iterate over all sequence numbers in the index bucket to
		// make sure we have the correct order of payments. Otherwise,
		// if we just loop over the payments bucket, we might get the
		// payments not in the chronological order but rather the
		// lexicographical order of the payment hashes.
		return indexes.ForEach(func(seqKey, indexVal []byte) error {
			// Progress reporting based on time + actual work done
			shouldReport := time.Since(lastReport) > 5*time.Second
			shouldReport = shouldReport ||
				stats.TotalPayments%100 == 0

			if shouldReport {
				elapsed := time.Since(startTime)
				paymentRate := float64(stats.TotalPayments) /
					elapsed.Seconds()
				attemptRate := float64(stats.TotalAttempts) /
					elapsed.Seconds()

				log.Infof("Progress: %d payments, %d "+
					"attempts, %d hops | Rate: %.1f "+
					"pmt/s, %.1f att/s | Elapsed: %v",
					stats.TotalPayments,
					stats.TotalAttempts, stats.TotalHops,
					paymentRate, attemptRate,
					elapsed.Round(time.Second),
				)
				lastReport = time.Now()
			}

			r := bytes.NewReader(indexVal)
			paymentHash, err := deserializePaymentIndex(r)
			if err != nil {
				return err
			}

			paymentBucket := paymentsBucket.NestedReadBucket(
				paymentHash[:],
			)
			if paymentBucket == nil {
				log.Warnf("Missing bucket for payment %x",
					paymentHash[:8])

				return nil
			}

			seqBytes := paymentBucket.Get(paymentSequenceKey)
			if seqBytes == nil {
				return ErrNoSequenceNumber
			}

			// Skip duplicates; they are migrated into the
			// payment_duplicates table when the primary payment is
			// processed.
			if !bytes.Equal(seqBytes, seqKey) {
				return nil
			}

			// Fetch the payment from the kv store.
			payment, err := fetchPayment(paymentBucket)
			if err != nil {
				return fmt.Errorf("fetch payment %x: %w",
					paymentHash[:8], err)
			}

			// Migrate the payment to the SQL database.
			paymentID, err := migratePayment(
				ctx, payment, paymentHash, sqlDB, stats,
			)
			if err != nil {
				return fmt.Errorf("migrate payment %x: %w",
					paymentHash[:8], err)
			}

			// Check for duplicates.
			dupBucket := paymentBucket.NestedReadBucket(
				duplicatePaymentsBucket,
			)
			if dupBucket != nil {
				err = migrateDuplicatePayments(
					ctx, dupBucket, paymentHash,
					paymentID, sqlDB, stats,
				)
				if err != nil {
					return fmt.Errorf("migrate duplicates "+
						"%x: %w", paymentHash[:8],
						err)
				}
			}

			// Add the payment to the validation batch.
			validationBatch = append(
				validationBatch, migratedPaymentRef{
					Hash:      paymentHash,
					PaymentID: paymentID,
				},
			)
			if uint32(len(validationBatch)) >=
				cfg.QueryCfg.MaxBatchSize {

				err := validateMigratedPaymentBatch(
					ctx, kvBackend, sqlDB,
					cfg,
					validationBatch,
				)
				if err != nil {
					return err
				}

				validatedPayments += int64(
					len(validationBatch),
				)
				log.Infof("Validated %d/%d payments",
					validatedPayments,
					stats.TotalPayments,
				)

				validationBatch = validationBatch[:0]
			}

			return nil
		})
	}, func() {})

	if err != nil {
		return fmt.Errorf("migrate payments: %w", err)
	}

	// Validate any remaining payments in the batch.
	if len(validationBatch) > 0 {
		if err := validateMigratedPaymentBatch(
			ctx, kvBackend, sqlDB, cfg, validationBatch,
		); err != nil {
			return err
		}

		validatedPayments += int64(len(validationBatch))
		log.Infof("Validated %d/%d payments", validatedPayments,
			stats.TotalPayments)
	}

	// Validate the total number of payments as an additional sanity check.
	if err := validatePaymentCounts(
		ctx, sqlDB, stats.TotalPayments,
	); err != nil {
		return err
	}

	stats.MigrationDuration = time.Since(startTime)

	printMigrationSummary(stats)

	return nil
}

// normalizeTimeForSQL converts a timestamp into the representation we persist
// and compare against in SQL:
// - drops any monotonic clock reading (SQL can't store it),
// - forces UTC for deterministic comparisons across environments.
//
// A zero time is returned unchanged.
func normalizeTimeForSQL(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}

	return time.Unix(0, t.UnixNano()).UTC()
}

// migratePayment migrates a single payment from KV to SQL.
func migratePayment(ctx context.Context, payment *MPPayment, hash lntypes.Hash,
	sqlDB SQLQueries, stats *MigrationStats) (int64, error) {

	// Update migration stats based on payment status.
	switch payment.Status {
	case StatusSucceeded:
		stats.SuccessfulPayments++

	case StatusFailed:
		stats.FailedPayments++

	case StatusInFlight:
		stats.InFlightPayments++
	}

	// Prepare fail reason for SQL insert.
	var failReason sql.NullInt32
	if payment.FailureReason != nil {
		failReason = sql.NullInt32{
			Int32: int32(*payment.FailureReason),
			Valid: true,
		}
	}

	// Insert payment using migration query.
	paymentID, err := sqlDB.InsertPaymentMig(
		ctx, sqlc.InsertPaymentMigParams{
			AmountMsat: int64(payment.Info.Value),
			CreatedAt: normalizeTimeForSQL(
				payment.Info.CreationTime,
			),
			PaymentIdentifier: hash[:],
			FailReason:        failReason,
		})
	if err != nil {
		return 0, fmt.Errorf("insert payment: %w", err)
	}

	// Insert payment intent.
	//
	// Only insert a row if we have an actual intent payload. For legacy
	// hash-only/keysend-style payments, the intent may be absent.
	if len(payment.Info.PaymentRequest) > 0 {
		_, err = sqlDB.InsertPaymentIntent(
			ctx, sqlc.InsertPaymentIntentParams{
				PaymentID:     paymentID,
				IntentType:    int16(PaymentIntentTypeBolt11),
				IntentPayload: payment.Info.PaymentRequest,
			},
		)
		if err != nil {
			return 0, fmt.Errorf("insert intent: %w", err)
		}
	}

	// Insert first hop custom records (payment level).
	for key, value := range payment.Info.FirstHopCustomRecords {
		err = sqlDB.InsertPaymentFirstHopCustomRecord(ctx,
			sqlc.InsertPaymentFirstHopCustomRecordParams{
				PaymentID: paymentID,
				Key:       int64(key),
				Value:     value,
			})
		if err != nil {
			return 0, fmt.Errorf("insert custom record: %w", err)
		}
	}

	// Migrate HTLC attempts.
	for _, htlc := range payment.HTLCs {
		err = migrateHTLCAttempt(
			ctx, paymentID, hash, &htlc, sqlDB, stats,
		)
		if err != nil {
			return 0, fmt.Errorf("migrate attempt %d: %w",
				htlc.AttemptID, err)
		}
	}

	stats.TotalPayments++

	return paymentID, nil
}

// migrateHTLCAttempt migrates a single HTLC attempt.
func migrateHTLCAttempt(ctx context.Context, paymentID int64,
	parentPaymentHash lntypes.Hash, htlc *HTLCAttempt,
	sqlDB SQLQueries, stats *MigrationStats) error {

	// Validate that we have a payment hash for the attempt.
	//
	// NOTE: We always require an attempt payment hash. A missing hash is an
	// unrecoverable inconsistency, because for AMP the hash may differ per
	// shard and for MPP/legacy payments the absence indicates corruption.
	var paymentHash []byte
	switch {
	case htlc.Hash != nil:
		paymentHash = (*htlc.Hash)[:]

	default:
		return fmt.Errorf("HTLC attempt %d missing payment hash "+
			"(parent payment hash=%x)", htlc.AttemptID,
			parentPaymentHash[:])
	}

	firstHopAmountMsat := int64(htlc.Route.FirstHopAmount.Val.Int())

	// Get the session key bytes.
	sessionKeyBytes := htlc.SessionKey().Serialize()

	// Insert HTLC attempt.
	_, err := sqlDB.InsertHtlcAttempt(ctx, sqlc.InsertHtlcAttemptParams{
		PaymentID:          paymentID,
		AttemptIndex:       int64(htlc.AttemptID),
		SessionKey:         sessionKeyBytes,
		AttemptTime:        normalizeTimeForSQL(htlc.AttemptTime),
		PaymentHash:        paymentHash,
		FirstHopAmountMsat: firstHopAmountMsat,
		RouteTotalTimeLock: int32(htlc.Route.TotalTimeLock),
		RouteTotalAmount:   int64(htlc.Route.TotalAmount),
		RouteSourceKey:     htlc.Route.SourcePubKey[:],
	})
	if err != nil {
		return fmt.Errorf("insert HTLC attempt: %w", err)
	}

	// Insert the route-level first hop custom records.
	for key, value := range htlc.Route.FirstHopWireCustomRecords {
		err = sqlDB.InsertPaymentAttemptFirstHopCustomRecord(
			ctx,
			sqlc.InsertPaymentAttemptFirstHopCustomRecordParams{
				HtlcAttemptIndex: int64(htlc.AttemptID),
				Key:              int64(key),
				Value:            value,
			},
		)
		if err != nil {
			return fmt.Errorf("insert attempt first hop custom "+
				"record: %w", err)
		}
	}

	// Insert route hops.
	for hopIndex := range htlc.Route.Hops {
		hop := htlc.Route.Hops[hopIndex]
		err = migrateRouteHop(
			ctx, int64(htlc.AttemptID), hopIndex, hop,
			sqlDB, stats,
		)
		if err != nil {
			return fmt.Errorf("migrate hop %d: %w", hopIndex, err)
		}
	}

	// Handle attempt resolution (settle or fail).
	switch {
	case htlc.Settle != nil:
		// Settled
		err = sqlDB.SettleAttempt(ctx, sqlc.SettleAttemptParams{
			AttemptIndex: int64(htlc.AttemptID),
			ResolutionTime: normalizeTimeForSQL(
				htlc.Settle.SettleTime,
			),
			ResolutionType: int32(HTLCAttemptResolutionSettled),
			SettlePreimage: htlc.Settle.Preimage[:],
		})
		if err != nil {
			return fmt.Errorf("settle attempt: %w", err)
		}

		stats.SettledAttempts++

	case htlc.Failure != nil:
		var failureMsg bytes.Buffer
		if htlc.Failure.Message != nil {
			err := lnwire.EncodeFailureMessage(
				&failureMsg, htlc.Failure.Message, 0,
			)
			if err != nil {
				return fmt.Errorf("failed to encode "+
					"failure message: %w", err)
			}
		}

		err = sqlDB.FailAttempt(ctx, sqlc.FailAttemptParams{
			AttemptIndex: int64(htlc.AttemptID),
			ResolutionTime: normalizeTimeForSQL(
				htlc.Failure.FailTime,
			),
			ResolutionType: int32(HTLCAttemptResolutionFailed),
			FailureSourceIndex: sql.NullInt32{
				Int32: int32(htlc.Failure.FailureSourceIndex),
				Valid: true,
			},
			HtlcFailReason: sql.NullInt32{
				Int32: int32(htlc.Failure.Reason),
				Valid: true,
			},
			FailureMsg: failureMsg.Bytes(),
		})
		if err != nil {
			return fmt.Errorf("fail attempt: %w", err)
		}

		stats.FailedAttempts++

	default:
		// If the attempt is not settled or failed, it is in flight.
		stats.InFlightAttempts++
	}

	stats.TotalAttempts++

	return nil
}

// migrateRouteHop migrates a single route hop.
func migrateRouteHop(ctx context.Context,
	attemptID int64, hopIndex int, hop *route.Hop, sqlDB SQLQueries,
	stats *MigrationStats) error {

	// Convert channel ID to string representation of uint64.
	// The SCID is stored as a decimal string to match the converter
	// expectations (sql_converters.go:173).
	scidStr := strconv.FormatUint(hop.ChannelID, 10)

	// Insert route hop.
	hopID, err := sqlDB.InsertRouteHop(ctx, sqlc.InsertRouteHopParams{
		HtlcAttemptIndex: attemptID,
		HopIndex:         int32(hopIndex),
		PubKey:           hop.PubKeyBytes[:],
		Scid:             scidStr,
		OutgoingTimeLock: int32(hop.OutgoingTimeLock),
		AmtToForward:     int64(hop.AmtToForward),
		MetaData:         hop.Metadata,
	})
	if err != nil {
		return fmt.Errorf("insert hop: %w", err)
	}

	// Check for blinded route data (route blinding).
	if len(hop.EncryptedData) > 0 || hop.BlindingPoint != nil ||
		hop.TotalAmtMsat != 0 {

		var blindingPoint []byte
		if hop.BlindingPoint != nil {
			blindingPoint = hop.BlindingPoint.SerializeCompressed()
		}

		var totalAmt sql.NullInt64
		if hop.TotalAmtMsat != 0 {
			totalAmt = sql.NullInt64{
				Int64: int64(hop.TotalAmtMsat),
				Valid: true,
			}
		}

		err := sqlDB.InsertRouteHopBlinded(
			ctx, sqlc.InsertRouteHopBlindedParams{
				HopID:               hopID,
				EncryptedData:       hop.EncryptedData,
				BlindingPoint:       blindingPoint,
				BlindedPathTotalAmt: totalAmt,
			},
		)
		if err != nil {
			return fmt.Errorf("insert blinded hop: %w", err)
		}
	}

	// Check for MPP record.
	if hop.MPP != nil {
		paymentAddr := hop.MPP.PaymentAddr()
		err = sqlDB.InsertRouteHopMpp(ctx, sqlc.InsertRouteHopMppParams{
			HopID:       hopID,
			PaymentAddr: paymentAddr[:],
			TotalMsat:   int64(hop.MPP.TotalMsat()),
		})
		if err != nil {
			return fmt.Errorf("insert MPP: %w", err)
		}
	}

	// Check for AMP record.
	if hop.AMP != nil {
		rootShare := hop.AMP.RootShare()
		setID := hop.AMP.SetID()
		err = sqlDB.InsertRouteHopAmp(ctx, sqlc.InsertRouteHopAmpParams{
			HopID:      hopID,
			RootShare:  rootShare[:],
			SetID:      setID[:],
			ChildIndex: int32(hop.AMP.ChildIndex()),
		})
		if err != nil {
			return fmt.Errorf("insert AMP: %w", err)
		}
	}

	// Check for custom records.
	if hop.CustomRecords != nil {
		for tlvType, value := range hop.CustomRecords {
			err = sqlDB.InsertPaymentHopCustomRecord(ctx,
				sqlc.InsertPaymentHopCustomRecordParams{
					HopID: hopID,
					Key:   int64(tlvType),
					Value: value,
				})
			if err != nil {
				return fmt.Errorf("insert hop custom "+
					"record: %w", err)
			}
		}
	}

	stats.TotalHops++

	return nil
}

// migrateDuplicatePayments migrates duplicate payments into the dedicated
// payment_duplicates table.
func migrateDuplicatePayments(ctx context.Context, dupBucket kvdb.RBucket,
	hash [32]byte, primaryPaymentID int64, sqlDB SQLQueries,
	stats *MigrationStats) error {

	duplicateCount := 0

	err := dupBucket.ForEach(func(seqBytes, _ []byte) error {
		// The duplicates bucket should only contain nested buckets
		// keyed by 8-byte sequence numbers. Skip any unexpected keys
		// (defensive check for corrupted or malformed data).
		if len(seqBytes) != 8 {
			log.Warnf("Skipping unexpected key in duplicates "+
				"bucket for payment %x: key length %d, "+
				"expected 8",
				hash[:8], len(seqBytes),
			)

			return nil
		}

		seqNum := byteOrder.Uint64(seqBytes)
		subBucket := dupBucket.NestedReadBucket(seqBytes)
		if subBucket == nil {
			return nil
		}

		duplicateCount++
		log.Infof("Migrating duplicate payment seq=%d for "+
			"payment %x", seqNum, hash[:8])

		err := migrateSingleDuplicatePayment(
			ctx, subBucket, hash, primaryPaymentID, seqNum,
			sqlDB,
		)
		if err != nil {
			return fmt.Errorf(
				"migrate duplicate payment seq=%d: %w",
				seqNum, err,
			)
		}

		return nil
	})

	if duplicateCount > 0 {
		stats.DuplicatePayments++
		stats.DuplicateEntries += int64(duplicateCount)

		log.Infof("Payment %x had %d duplicate(s) migrated", hash[:8],
			duplicateCount)
	}

	return err
}

// migrateSingleDuplicatePayment inserts a duplicate payment record for the
// given payment hash into payment_duplicates.
func migrateSingleDuplicatePayment(ctx context.Context, dupBucket kvdb.RBucket,
	hash [32]byte, primaryPaymentID int64, duplicateSeq uint64,
	sqlDB SQLQueries) error {

	creationData := dupBucket.Get(duplicatePaymentCreationInfoKey)
	if creationData == nil {
		return fmt.Errorf("duplicate payment seq=%d missing "+
			"creation info (payment=%x)", duplicateSeq, hash[:8])
	}

	creationInfo, err := deserializeDuplicatePaymentCreationInfo(
		bytes.NewReader(creationData),
	)
	if err != nil {
		return fmt.Errorf("deserialize duplicate creation "+
			"info: %w", err)
	}

	settleData := dupBucket.Get(duplicatePaymentSettleInfoKey)
	failReasonData := dupBucket.Get(duplicatePaymentFailInfoKey)
	attemptData := dupBucket.Get(duplicatePaymentAttemptInfoKey)

	if settleData != nil && len(failReasonData) > 0 {
		return fmt.Errorf("duplicate payment seq=%d has both "+
			"settle and fail info (payment=%x)", duplicateSeq,
			hash[:8])
	}

	var (
		failReason     sql.NullInt32
		settlePreimage []byte
		settleTime     sql.NullTime
	)

	switch {
	case settleData != nil:
		settlePreimage, settleTime, err = parseDuplicateSettleData(
			settleData,
		)
		if err != nil {
			return err
		}

	case len(failReasonData) > 0:
		failReason = sql.NullInt32{
			Int32: int32(failReasonData[0]),
			Valid: true,
		}

	default:
		if attemptData == nil {
			log.Warnf("Duplicate payment seq=%d has no "+
				"attempt info and no resolution (payment=%x); "+
				"marking failed", duplicateSeq, hash[:8])
		} else {
			log.Warnf("Duplicate payment seq=%d has attempt "+
				"info but no resolution (payment=%x); "+
				"marking failed", duplicateSeq, hash[:8])
		}

		failReason = sql.NullInt32{
			Int32: int32(FailureReasonError),
			Valid: true,
		}
	}

	_, err = sqlDB.InsertPaymentDuplicateMig(
		ctx, sqlc.InsertPaymentDuplicateMigParams{
			PaymentID:         primaryPaymentID,
			PaymentIdentifier: creationInfo.PaymentIdentifier[:],
			AmountMsat:        int64(creationInfo.Value),
			CreatedAt: normalizeTimeForSQL(
				creationInfo.CreationTime,
			),
			FailReason:     failReason,
			SettlePreimage: settlePreimage,
			SettleTime:     settleTime,
		},
	)
	if err != nil {
		return fmt.Errorf("insert duplicate payment: %w", err)
	}

	return nil
}

// parseDuplicateSettleData extracts settle data from either legacy or modern
// duplicate formats.
func parseDuplicateSettleData(settleData []byte) ([]byte, sql.NullTime, error) {
	if len(settleData) == lntypes.PreimageSize {
		return append([]byte(nil), settleData...), sql.NullTime{}, nil
	}

	settleInfo, err := deserializeHTLCSettleInfo(
		bytes.NewReader(settleData),
	)
	if err != nil {
		return nil, sql.NullTime{},
			fmt.Errorf("deserialize duplicate settle: %w", err)
	}

	settleTime := normalizeTimeForSQL(settleInfo.SettleTime)

	return settleInfo.Preimage[:], sql.NullTime{
		Time:  settleTime,
		Valid: !settleTime.IsZero(),
	}, nil
}

// printMigrationSummary prints a summary of the migration.
func printMigrationSummary(stats *MigrationStats) {
	log.Infof("========================================")
	log.Infof("   Payment Migration Summary")
	log.Infof("========================================")
	log.Infof("Total Payments:        %d", stats.TotalPayments)
	log.Infof("  Successful:       %d", stats.SuccessfulPayments)
	log.Infof("  Failed:           %d", stats.FailedPayments)
	log.Infof("  In-Flight:        %d", stats.InFlightPayments)
	log.Infof("")
	log.Infof("Total HTLC Attempts:   %d", stats.TotalAttempts)
	log.Infof("  Settled:          %d", stats.SettledAttempts)
	log.Infof("  Failed:           %d", stats.FailedAttempts)
	log.Infof("  In-Flight:        %d", stats.InFlightAttempts)
	log.Infof("")
	log.Infof("Total Route Hops:      %d", stats.TotalHops)

	if stats.DuplicatePayments > 0 {
		log.Infof("")
		log.Warnf("DUPLICATE PAYMENTS DETECTED:")
		log.Warnf("  Unique payment hashes with duplicates: %d",
			stats.DuplicatePayments)
		log.Warnf("  Total duplicate entries migrated:      %d",
			stats.DuplicateEntries)
		log.Warnf("  These were caused by an old LND bug.")
	}

	log.Infof("")
	log.Infof("Migration Duration:    %v", stats.MigrationDuration)
	log.Infof("========================================")
}
