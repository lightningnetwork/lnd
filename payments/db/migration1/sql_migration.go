package migration1

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/payments/db/migration1/lnwire"
	"github.com/lightningnetwork/lnd/payments/db/migration1/sqlc"
	"golang.org/x/time/rate"
)

const (
	// defaultRateWindowDuration is the rolling window used for ETA
	// calculation during migration progress reporting.
	defaultRateWindowDuration = 300 * time.Second
)

var (
	// switchNextPaymentIDKey is the switch sequencer bucket key. This is
	// intentionally kept in sync with htlcswitch.nextPaymentIDKey without
	// importing htlcswitch into the migration package.
	switchNextPaymentIDKey = []byte("next-payment-id-key")
)

// MigrationStats tracks migration progress.
type MigrationStats struct {
	TotalPayments      int64
	SuccessfulPayments int64
	FailedPayments     int64
	InFlightPayments   int64
	InitiatedPayments  int64
	TotalAttempts      int64
	SettledAttempts    int64
	FailedAttempts     int64
	InFlightAttempts   int64
	TotalHops          int64
	DuplicatePayments  int64
	DuplicateEntries   int64
	SkippedPayments    int64
	MigrationDuration  time.Duration
}

// migrationProgressReporter tracks rolling-window ETA state and logs
// periodic progress lines during the payment migration.
type migrationProgressReporter struct {
	startTime          time.Time
	stats              *MigrationStats
	indexedPayments    int64
	rateWindowDuration time.Duration
	windowStart        time.Time
	windowPayments     int64
	prevWindowRate     float64
}

// report logs a progress line showing the current migration rate and ETA.
func (p *migrationProgressReporter) report() {
	elapsed := time.Since(p.startTime)
	if elapsed <= 0 || p.stats.TotalPayments == 0 {
		return
	}

	paymentRate := float64(p.stats.TotalPayments) / elapsed.Seconds()
	attemptRate := float64(p.stats.TotalAttempts) / elapsed.Seconds()

	var pctStr string
	if p.indexedPayments > 0 {
		pct := float64(p.stats.TotalPayments) /
			float64(p.indexedPayments) * 100
		pctStr = fmt.Sprintf(" (~%.1f%%)", pct)
	}

	// Compute ETA using the rolling window rate so it responds to
	// recent throughput changes. When the window expires we save
	// the previous rate as a fallback for the reset tick.
	windowElapsed := time.Since(p.windowStart)
	if windowElapsed >= p.rateWindowDuration {
		n := p.stats.TotalPayments - p.windowPayments
		p.prevWindowRate = float64(n) / windowElapsed.Seconds()
		p.windowPayments = p.stats.TotalPayments
		p.windowStart = time.Now()
		windowElapsed = 0
	}

	var etaStr string
	if p.indexedPayments > 0 {
		windowRate := p.prevWindowRate
		if windowElapsed > 0 {
			n := p.stats.TotalPayments - p.windowPayments
			windowRate = float64(n) / windowElapsed.Seconds()
		}

		if windowRate > 0 {
			remaining := p.indexedPayments - p.stats.TotalPayments
			secs := float64(remaining) / windowRate
			eta := time.Duration(secs) * time.Second
			etaStr = fmt.Sprintf(
				" | ETA: ~%v", eta.Round(time.Second),
			)
		}
	}

	log.Infof("Progress: %d payments%s, %d attempts, %d hops | Rate: %.1f "+
		"pmt/s, %.1f att/s | Elapsed: %v%s", p.stats.TotalPayments,
		pctStr, p.stats.TotalAttempts, p.stats.TotalHops,
		paymentRate, attemptRate, elapsed.Round(time.Second), etaStr)
}

// MigratePaymentsKVToSQL migrates payments from KV to SQL and validates
// migrated data in batches. Callers are responsible for executing this within
// a single SQL transaction if atomicity is required.
func MigratePaymentsKVToSQL(ctx context.Context, kvBackend kvdb.Backend,
	sqlDB SQLMigrationQueries, cfg *SQLStoreConfig) error {

	if cfg == nil {
		return fmt.Errorf("missing SQL store config for migration")
	}

	if cfg.QueryCfg == nil {
		return fmt.Errorf("missing SQL store config for validation")
	}

	if cfg.QueryCfg.MaxBatchSize == 0 {
		return fmt.Errorf("invalid max batch size for validation")
	}

	stats := &MigrationStats{}
	startTime := time.Now()

	log.Infof("Starting payment migration from KV to SQL...")

	var (
		validationBatch []migratedPaymentRef

		reportInterval = rate.Sometimes{Interval: 5 * time.Second}
	)

	indexedPayments, nextSwitchPaymentID, err := collectMigrationState(
		kvBackend,
	)
	if err != nil {
		return fmt.Errorf("collect payment migration state: %w", err)
	}

	attemptIDAllocator := newAttemptIDAllocator(nextSwitchPaymentID)

	log.Infof("Found ~%d index entries to migrate (includes duplicates)",
		indexedPayments)

	// Set up a progress reporter with rolling-window ETA.
	reporter := &migrationProgressReporter{
		startTime:          startTime,
		stats:              stats,
		indexedPayments:    indexedPayments,
		rateWindowDuration: defaultRateWindowDuration,
		windowStart:        startTime,
	}

	// Open the KV backend in read-only mode.
	err = kvBackend.View(func(kvTx kvdb.RTx) error {
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
			reportInterval.Do(reporter.report)

			return migrateIndexEntry(
				ctx, seqKey, indexVal, paymentsBucket,
				kvBackend, sqlDB, cfg, stats, &validationBatch,
				attemptIDAllocator,
			)
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
	}

	// Validate the total number of payments as an additional sanity check.
	if err := validatePaymentCounts(
		ctx, sqlDB, stats.TotalPayments,
	); err != nil {
		return err
	}

	if err := advanceSwitchPaymentIDSequence(
		kvBackend, attemptIDAllocator.nextID,
	); err != nil {
		return fmt.Errorf("advance switch payment ID sequence: %w", err)
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

// collectMigrationState scans the payment index once to gather progress
// information and reads the switch sequencer horizon used for legacy attempt
// ID allocation.
func collectMigrationState(kvBackend kvdb.Backend) (int64, uint64, error) {
	var (
		indexedPayments     int64
		nextSwitchPaymentID uint64
	)
	err := kvBackend.View(func(kvTx kvdb.RTx) error {
		// Read the switch sequencer horizon that legacy zero-ID
		// attempts will allocate from if needed.
		seqBucket := kvTx.ReadBucket(switchNextPaymentIDKey)
		if seqBucket != nil {
			nextSwitchPaymentID = seqBucket.Sequence()
		}

		// If there are no payments, there is nothing to count or
		// migrate.
		paymentsBucket := kvTx.ReadBucket(paymentsRootBucket)
		if paymentsBucket == nil {
			log.Infof("No payments bucket found - database is " +
				"empty")

			return nil
		}

		// Count index entries for approximate progress reporting. The
		// main migration still streams over this index in order.
		indexes := kvTx.ReadBucket(paymentsIndexBucket)
		if indexes == nil {
			return fmt.Errorf("index bucket does not exist")
		}

		return indexes.ForEach(func(_, _ []byte) error {
			indexedPayments++
			return nil
		})
	}, func() {
		indexedPayments = 0
		nextSwitchPaymentID = 0
	})
	if err != nil {
		return 0, 0, err
	}

	return indexedPayments, nextSwitchPaymentID, nil
}

// attemptIDAllocator tracks the next switch payment ID that is safe to hand
// out after migration.
type attemptIDAllocator struct {
	// nextID is the in-memory counter used for the next synthetic attempt
	// ID and the final switch sequencer horizon to persist.
	nextID uint64
}

// newAttemptIDAllocator creates a new attempt ID allocator.
//
// The SQL schema requires payment_htlc_attempts.attempt_index to be globally
// unique because attempt-related rows use it as their stable identifier. Very
// old KV payments can contain attempt ID zero, which represented an unknown
// legacy value and cannot be preserved in SQL without colliding with every
// other such legacy attempt.
//
// Non-zero attempt IDs were allocated from the switch sequencer. The sequencer
// persists a horizon: it reads Sequence() as the next ID to hand out, then
// writes a higher value before returning that ID. This means the value stored
// in switchNextPaymentIDKey is already beyond all IDs it handed out. We
// therefore allocate replacement IDs for legacy zero attempts from that horizon
// and persist the final next-unused value once migration succeeds. These old
// attempts may receive high attempt_index values, which means that within a
// payment that mixes remapped and non-remapped attempts the remapped ones will
// sort after the originals when SQL queries order by attempt_index. This is
// acceptable because it only affects very old payments whose attempt IDs were
// already unknown, and intra-payment attempt ordering is not a load-bearing
// user-visible invariant; uniqueness and future non-collision with the switch
// sequencer are the actual invariants we need to preserve.
func newAttemptIDAllocator(nextSwitchPaymentID uint64) *attemptIDAllocator {
	return &attemptIDAllocator{
		nextID: nextSwitchPaymentID,
	}
}

// allocateLegacyAttemptID returns a new unique attempt ID for a legacy payment.
// It uses the in-memory counter initialized from the switch payment ID
// sequencer horizon.
func (a *attemptIDAllocator) allocateLegacyAttemptID() (uint64, error) {
	// The runtime switch sequencer never hands out ID zero: when its
	// persisted sequence is zero, it starts by issuing ID one. Mirror
	// that behavior for legacy attempts on otherwise idle nodes whose
	// switch sequencer bucket has not allocated a batch yet.
	if a.nextID == 0 {
		a.nextID = 1
	}

	if a.nextID == ^uint64(0) {
		return 0, fmt.Errorf("cannot allocate legacy attempt ID: "+
			"switch payment ID sequence is %d", a.nextID)
	}

	attemptID := a.nextID
	a.nextID++

	return attemptID, nil
}

// advanceSwitchPaymentIDSequence makes sure the switch sequencer cannot later
// hand out an ID that was already present in a migrated payment attempt.
func advanceSwitchPaymentIDSequence(kvBackend kvdb.Backend,
	nextID uint64) error {

	return kvdb.Update(kvBackend, func(tx kvdb.RwTx) error {
		seqBucket := tx.ReadWriteBucket(switchNextPaymentIDKey)
		if seqBucket == nil {
			if nextID <= 1 {
				return nil
			}

			var err error
			seqBucket, err = tx.CreateTopLevelBucket(
				switchNextPaymentIDKey,
			)
			if err != nil {
				return err
			}
		}

		currentSeq := seqBucket.Sequence()
		if currentSeq == nextID {
			// No synthetic IDs were allocated, so the sequencer is
			// already at the migration cursor.
			return nil
		}
		if currentSeq > nextID {
			// Migration runs exclusively, so the sequencer should
			// not move beyond the cursor computed by migration.
			return fmt.Errorf("switch payment ID sequence above "+
				"migration horizon: current=%d, expected=%d",
				currentSeq, nextID)
		}

		// Synthetic IDs were allocated, so advance the sequencer to
		// the final next-unused ID.
		return seqBucket.SetSequence(nextID)
	}, func() {})
}

// migrateIndexEntry processes a single entry from the payments index bucket,
// migrating the corresponding payment to SQL and appending it to the
// validation batch.
func migrateIndexEntry(ctx context.Context, seqKey, indexVal []byte,
	paymentsBucket kvdb.RBucket, kvBackend kvdb.Backend,
	sqlDB SQLMigrationQueries, cfg *SQLStoreConfig, stats *MigrationStats,
	validationBatch *[]migratedPaymentRef,
	attemptIDAllocator *attemptIDAllocator) error {

	r := bytes.NewReader(indexVal)
	paymentHash, err := deserializePaymentIndex(r)
	if err != nil {
		return err
	}

	paymentBucket := paymentsBucket.NestedReadBucket(paymentHash[:])
	if paymentBucket == nil {
		// We skip the entry in case this sequence number does not
		// have a corresponding payment bucket. But aborting would
		// not help either because it is just a db inconsistency.
		log.Warnf("Missing bucket for payment %x", paymentHash[:8])
		stats.SkippedPayments++

		return nil
	}

	// Every payment bucket should have a sequence number which is
	// also important to check for duplicates.
	seqBytes := paymentBucket.Get(paymentSequenceKey)
	if seqBytes == nil {
		return ErrNoSequenceNumber
	}

	// Skip duplicates. They are migrated into the payment_duplicates
	// table when the primary payment is processed.
	if !bytes.Equal(seqBytes, seqKey) {
		return nil
	}

	// Fetch the payment from the kv store.
	payment, err := fetchPayment(paymentBucket)
	if err != nil {
		return fmt.Errorf("fetch payment %x: %w", paymentHash[:8], err)
	}

	// Migrate the payment to the SQL database.
	paymentID, err := migratePayment(
		ctx, payment, paymentHash, sqlDB, stats, attemptIDAllocator,
	)
	if err != nil {
		return fmt.Errorf("migrate payment %x: %w", paymentHash[:8],
			err)
	}

	// Migrate any duplicate payments for this hash.
	dupBucket := paymentBucket.NestedReadBucket(duplicatePaymentsBucket)
	if dupBucket != nil {
		err = migrateDuplicatePayments(
			ctx, dupBucket, paymentHash, paymentID, sqlDB, stats,
		)
		if err != nil {
			return fmt.Errorf("migrate duplicates %x: %w",
				paymentHash[:8], err)
		}
	}

	// Add the payment to the validation batch.
	*validationBatch = append(*validationBatch, migratedPaymentRef{
		Hash:      paymentHash,
		PaymentID: paymentID,
	})
	if uint32(len(*validationBatch)) >= cfg.QueryCfg.MaxBatchSize {
		err := validateMigratedPaymentBatch(
			ctx, kvBackend, sqlDB, cfg, *validationBatch,
		)
		if err != nil {
			return err
		}

		*validationBatch = (*validationBatch)[:0]
	}

	return nil
}

// migratePayment migrates a single payment from KV to SQL.
func migratePayment(ctx context.Context, payment *MPPayment, hash lntypes.Hash,
	sqlDB SQLMigrationQueries, stats *MigrationStats,
	attemptIDAllocator *attemptIDAllocator) (int64, error) {

	if payment.Status == StatusInFlight {
		terminalizedLegacyAttempts, err :=
			terminalizeUnresolvedLegacyZeroAttempts(payment)
		if err != nil {
			return 0, err
		}
		if terminalizedLegacyAttempts > 0 {
			log.Warnf("Terminalized %d unresolved legacy HTLC "+
				"attempt(s) with unknown attempt ID zero for "+
				"payment %x; the parent payment was failed if "+
				"no other settled or in-flight HTLC kept "+
				"it active", terminalizedLegacyAttempts,
				hash[:8])
		}
	}

	// Update migration stats based on payment status.
	switch payment.Status {
	case StatusSucceeded:
		stats.SuccessfulPayments++

	case StatusFailed:
		stats.FailedPayments++

	case StatusInFlight:
		stats.InFlightPayments++

	case StatusInitiated:
		stats.InitiatedPayments++
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
			},
		)
		if err != nil {
			return 0, fmt.Errorf("insert custom record: %w", err)
		}
	}

	// Migrate HTLC attempts.
	for _, htlc := range payment.HTLCs {
		err = migrateHTLCAttempt(
			ctx, paymentID, hash, &htlc, sqlDB, stats,
			attemptIDAllocator,
		)
		if err != nil {
			return 0, fmt.Errorf("migrate attempt %d: %w",
				htlc.AttemptID, err)
		}
	}

	stats.TotalPayments++

	return paymentID, nil
}

// terminalizeUnresolvedLegacyZeroAttempts marks unresolved legacy zero-ID HTLC
// attempts failed and fails the parent payment if no other resolved or
// recoverable in-flight HTLC keeps it active.
//
// Attempt ID zero was written by an old KV migration as an unknown legacy
// value. If such an attempt has no settle/fail resolution, it cannot be safely
// resumed after SQL migration because the live switch state would not know the
// synthetic attempt ID assigned below.
//
// Callers only need this for in-flight payments: any unresolved HTLC makes the
// parent payment in-flight, while terminal historical payments can use the
// regular zero-ID remap path.
func terminalizeUnresolvedLegacyZeroAttempts(payment *MPPayment) (int, error) {
	var (
		terminalizedLegacyAttempts int
		hasSettled                 bool
		hasNonZeroInFlight         bool
	)

	for i := range payment.HTLCs {
		htlc := &payment.HTLCs[i]
		switch {
		case htlc.Settle != nil:
			hasSettled = true

		case htlc.Failure != nil:

		case htlc.AttemptID == 0:
			htlc.Failure = &HTLCFailInfo{
				Reason: HTLCFailUnknown,
			}
			terminalizedLegacyAttempts++

		default:
			hasNonZeroInFlight = true
		}
	}

	if terminalizedLegacyAttempts == 0 {
		return 0, nil
	}

	if !hasSettled && !hasNonZeroInFlight && payment.FailureReason == nil {
		reason := FailureReasonError
		payment.FailureReason = &reason
	}

	if err := payment.setState(); err != nil {
		return 0, err
	}

	return terminalizedLegacyAttempts, nil
}

// migrateHTLCAttempt migrates a single HTLC attempt.
func migrateHTLCAttempt(ctx context.Context, paymentID int64,
	parentPaymentHash lntypes.Hash, htlc *HTLCAttempt, sqlDB SQLQueries,
	stats *MigrationStats, attemptIDAllocator *attemptIDAllocator) error {

	// Determine the payment hash for this HTLC attempt.
	//
	// For AMP payments, each HTLC has its own unique hash. For non-AMP
	// payments (MPP, Legacy), all HTLCs use the same hash as the parent
	// payment. Older payment attempts may not have the hash stored
	// explicitly, in which case we fall back to the parent payment hash
	// which is ok since non-AMP payments have a single hash for all HTLCs.
	var paymentHash []byte
	switch {
	case htlc.Hash != nil:
		paymentHash = (*htlc.Hash)[:]

	default:
		// For older payments where Hash is nil, use the parent payment
		// hash. This is consistent with how the router handles these
		// legacy payments.
		paymentHash = parentPaymentHash[:]
	}

	firstHopAmountMsat := int64(htlc.Route.FirstHopAmount.Val.Int())

	sessionKey := htlc.SessionKey()
	if sessionKey == nil {
		return fmt.Errorf("HTLC attempt %d for payment %x is "+
			"missing session key", htlc.AttemptID,
			parentPaymentHash[:8])
	}

	sessionKeyBytes := sessionKey.Serialize()

	attemptID := htlc.AttemptID
	if attemptID == 0 {
		var err error
		attemptID, err = attemptIDAllocator.allocateLegacyAttemptID()
		if err != nil {
			return fmt.Errorf("allocate legacy attempt ID: %w", err)
		}

		log.Warnf("Allocated HTLC attempt index %d from switch "+
			"sequencer for legacy payment %x with unknown "+
			"attempt ID", attemptID,
			parentPaymentHash[:8])
	}

	if attemptID > math.MaxInt64 {
		return fmt.Errorf("unable to convert HTLC attempt ID to "+
			"SQL attempt index: attempt_id=%d payment=%x max=%d",
			attemptID, parentPaymentHash[:8], uint64(math.MaxInt64))
	}

	attemptIndex := int64(attemptID)

	// Insert HTLC attempt.
	_, err := sqlDB.InsertHtlcAttempt(ctx, sqlc.InsertHtlcAttemptParams{
		PaymentID:          paymentID,
		AttemptIndex:       attemptIndex,
		SessionKey:         sessionKeyBytes,
		AttemptTime:        normalizeTimeForSQL(htlc.AttemptTime),
		PaymentHash:        paymentHash,
		FirstHopAmountMsat: firstHopAmountMsat,
		RouteTotalTimeLock: int32(htlc.Route.TotalTimeLock),
		RouteTotalAmount:   int64(htlc.Route.TotalAmount),
		RouteSourceKey:     htlc.Route.SourcePubKey[:],
	})
	if err != nil {
		// SQL unique constraint errors do not include the conflicting
		// value. Include the attempted index so failures point directly
		// at the problematic legacy attempt.
		return fmt.Errorf("unable to insert HTLC attempt: "+
			"index=%d payment=%x original_attempt_id=%d: %w",
			attemptIndex, parentPaymentHash[:8],
			htlc.AttemptID, err)
	}

	// Insert the route-level first hop custom records.
	for key, value := range htlc.Route.FirstHopWireCustomRecords {
		err = sqlDB.InsertPaymentAttemptFirstHopCustomRecord(
			ctx,
			sqlc.InsertPaymentAttemptFirstHopCustomRecordParams{
				HtlcAttemptIndex: attemptIndex,
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

		// Use the parent hash for diagnostics. For AMP payments this
		// is the set ID, which identifies the payment containing the
		// shard.
		err = migrateRouteHop(
			ctx, parentPaymentHash, attemptIndex, hopIndex, hop,
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
			AttemptIndex: attemptIndex,
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
			AttemptIndex: attemptIndex,
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
	parentPaymentHash lntypes.Hash, attemptIndex int64, hopIndex int,
	hop *Hop, sqlDB SQLQueries, stats *MigrationStats) error {

	// Convert channel ID to string representation of uint64.
	// The SCID is stored as a decimal string to match the converter
	// expectations (sql_converters.go:173).
	scidStr := strconv.FormatUint(hop.ChannelID, 10)

	// Insert route hop.
	hopID, err := sqlDB.InsertRouteHop(ctx, sqlc.InsertRouteHopParams{
		HtlcAttemptIndex: attemptIndex,
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

	// Non-empty encrypted recipient data identifies a blinded hop.
	// The RPC boundary has required encrypted data with a blinding point
	// since these fields were introduced. Internally built blinded routes
	// also always contain both. Unlike an orphaned total, a point without
	// encrypted data is not a supported legacy encoding. Report it as
	// malformed instead of silently discarding it. Keep this rejection in
	// sync with normalizePaymentForCompare, which only normalizes hops
	// without a blinding point.
	hasEncryptedData := len(hop.EncryptedData) > 0
	if !hasEncryptedData && hop.BlindingPoint != nil {
		return fmt.Errorf("invalid blinded hop: payment_hash=%x, "+
			"attempt_index=%d, hop=%d: blinding point requires "+
			"encrypted recipient data", parentPaymentHash[:8],
			attemptIndex, hopIndex)
	}

	// SendToRouteV2 historically allowed a blinded total amount without
	// blinded hop data. Omit such an orphaned total rather than creating a
	// blinded-hop row.
	if !hasEncryptedData && hop.TotalAmtMsat != 0 {
		log.Warnf("Ignoring orphaned blinded total amount: "+
			"payment_hash=%x, attempt_index=%d, hop=%d, "+
			"total_amt_msat=%d", parentPaymentHash[:8],
			attemptIndex, hopIndex, hop.TotalAmtMsat)
	}

	// The blinding point and total amount are only associated fields. Use
	// the length so nil and empty encrypted data are handled consistently.
	if hasEncryptedData {
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
			err = sqlDB.InsertPaymentHopCustomRecord(
				ctx,
				sqlc.InsertPaymentHopCustomRecordParams{
					HopID: hopID,
					Key:   int64(tlvType),
					Value: value,
				},
			)
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
	hash [32]byte, primaryPaymentID int64, sqlDB SQLMigrationQueries,
	stats *MigrationStats) error {

	duplicateCount := 0

	err := dupBucket.ForEach(func(seqBytes, _ []byte) error {
		// The duplicates bucket should only contain nested buckets
		// keyed by 8-byte sequence numbers. Skip any unexpected keys
		// (defensive check for corrupted or malformed data).
		if len(seqBytes) != 8 {
			log.Warnf("Skipping unexpected key in duplicates "+
				"bucket for payment %x: key length %d, "+
				"expected 8", hash[:8], len(seqBytes))

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
			return fmt.Errorf("migrate duplicate payment "+
				"seq=%d: %w", seqNum, err)
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
	sqlDB SQLMigrationQueries) error {

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
		// If the duplicate payment has no settle or fail info,
		// we mark it as failed during the migration. Duplicate
		// payments were a bug in older versions of LND, so we can be
		// sure if a duplicate payment has no failure reason or
		// settlement data, the corresponding HTLC for this payment
		// has been failed (resolved).
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
			PaymentID:  primaryPaymentID,
			AmountMsat: int64(creationInfo.Value),
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
	if stats.TotalPayments == 0 {
		log.Infof("No payments migrated - database is empty")

		return
	}

	log.Infof("========================================")
	log.Infof("   Payment Migration Summary")
	log.Infof("========================================")
	log.Infof("Total Payments:        %d", stats.TotalPayments)
	log.Infof("  Successful:       %d", stats.SuccessfulPayments)
	log.Infof("  Failed:           %d", stats.FailedPayments)
	log.Infof("  In-Flight:        %d", stats.InFlightPayments)
	log.Infof("  Initiated:        %d", stats.InitiatedPayments)
	log.Infof("")
	log.Infof("Total HTLC Attempts:   %d", stats.TotalAttempts)
	log.Infof("  Settled:          %d", stats.SettledAttempts)
	log.Infof("  Failed:           %d", stats.FailedAttempts)
	log.Infof("  In-Flight:        %d", stats.InFlightAttempts)
	log.Infof("")
	log.Infof("Total Route Hops:      %d", stats.TotalHops)

	if stats.SkippedPayments > 0 {
		log.Infof("")
		log.Warnf("SKIPPED PAYMENTS:")
		log.Warnf("  Indexed payments with missing buckets: %d",
			stats.SkippedPayments)
		log.Warnf("  These indicate minor DB inconsistencies.")
	}

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
