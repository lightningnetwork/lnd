package migration1

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/payments/db/migration1/sqlc"
	"github.com/pmezard/go-difflib/difflib"
)

type migratedPaymentRef struct {
	Hash      lntypes.Hash
	PaymentID int64
}

// validateMigratedPaymentBatch performs a deep validation pass by comparing
// KV payments with their SQL counterparts for a batch of payments.
func validateMigratedPaymentBatch(ctx context.Context,
	kvBackend kvdb.Backend, sqlDB SQLQueries,
	cfg *SQLStoreConfig, batch []migratedPaymentRef) error {

	if len(batch) == 0 {
		return nil
	}

	if cfg == nil || cfg.QueryCfg == nil {
		return fmt.Errorf("missing SQL store config for validation")
	}

	paymentIDs := make([]int64, 0, len(batch))
	for _, item := range batch {
		paymentIDs = append(paymentIDs, item.PaymentID)
	}

	rows, err := sqlDB.FetchPaymentsByIDs(ctx, paymentIDs)
	if err != nil {
		return fmt.Errorf("fetch SQL payments: %w", err)
	}
	if len(rows) != len(paymentIDs) {
		return fmt.Errorf("SQL payment batch mismatch: got=%d want=%d",
			len(rows), len(paymentIDs))
	}

	batchData, err := batchLoadPaymentDetailsData(
		ctx, cfg.QueryCfg, sqlDB, paymentIDs,
	)
	if err != nil {
		return fmt.Errorf("load payment batch: %w", err)
	}

	err = kvBackend.View(func(kvTx kvdb.RTx) error {
		paymentsBucket := kvTx.ReadBucket(paymentsRootBucket)
		if paymentsBucket == nil {
			return fmt.Errorf("no payments bucket")
		}

		for _, row := range rows {
			payment := row.GetPayment()
			hash := payment.PaymentIdentifier
			var paymentHash lntypes.Hash
			copy(paymentHash[:], hash)

			paymentBucket := paymentsBucket.NestedReadBucket(hash)
			if paymentBucket == nil {
				return fmt.Errorf("missing payment bucket %x",
					hash[:8])
			}

			kvPayment, err := fetchPayment(paymentBucket)
			if err != nil {
				return fmt.Errorf("fetch KV payment %x: %w",
					hash[:8], err)
			}

			sqlPayment, err := buildPaymentFromBatchData(
				row, batchData,
			)
			if err != nil {
				return fmt.Errorf("build SQL payment %x: %w",
					hash[:8], err)
			}

			normalizePaymentForCompare(kvPayment)
			normalizePaymentForCompare(sqlPayment)

			if !reflect.DeepEqual(kvPayment, sqlPayment) {
				// make sure we properly print the diff between
				// the two payments if they are not equal.
				dumpCfg := spew.ConfigState{
					DisablePointerAddresses: true,
					DisableCapacities:       true,
					DisableMethods:          true,
					SortKeys:                true,
				}
				diff := difflib.UnifiedDiff{
					A: difflib.SplitLines(
						dumpCfg.Sdump(kvPayment),
					),
					B: difflib.SplitLines(
						dumpCfg.Sdump(sqlPayment),
					),
					FromFile: "kv",
					ToFile:   "sql",
					Context:  3,
				}
				diffText, _ := difflib.GetUnifiedDiffString(
					diff,
				)

				return fmt.Errorf("payment mismatch %x\n%s",
					hash[:8], diffText)
			}

			err = compareDuplicatePayments(
				ctx, paymentBucket, sqlDB, payment.ID,
				paymentHash,
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	if err != nil {
		return err
	}

	return nil
}

// normalizePaymentForCompare normalizes fields that are expected to differ
// between KV and SQL representations before deep comparison.
func normalizePaymentForCompare(payment *MPPayment) {
	if payment == nil {
		return
	}

	// SequenceNum will not be equal because the kv db can have already
	// payments deleted during its lifetime.
	payment.SequenceNum = 0

	trunc := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}

		return time.Unix(0, t.UnixNano()).
			In(time.Local).
			Truncate(time.Microsecond)
	}

	// Normalize PaymentCreationInfo fields.
	if payment.Info != nil {
		payment.Info.CreationTime = trunc(
			payment.Info.CreationTime,
		)
		if len(payment.Info.PaymentRequest) == 0 {
			payment.Info.PaymentRequest = nil
		}
		if len(payment.Info.FirstHopCustomRecords) == 0 {
			payment.Info.FirstHopCustomRecords = nil
		}
	}

	// Normalize HTLCAttemptInfo list to empty if it is nil.
	if len(payment.HTLCs) == 0 {
		payment.HTLCs = []HTLCAttempt{}
	}

	// Normalize HTLC attempt ordering; SQL/KV may return attempts
	// in different orders.
	sort.SliceStable(payment.HTLCs, func(i, j int) bool {
		if payment.HTLCs[i].AttemptID == payment.HTLCs[j].AttemptID {
			return payment.HTLCs[i].AttemptTime.Before(
				payment.HTLCs[j].AttemptTime,
			)
		}

		return payment.HTLCs[i].AttemptID < payment.HTLCs[j].AttemptID
	})

	// Normalize HTLCAttemptInfo fields.
	for i := range payment.HTLCs {
		htlc := &payment.HTLCs[i]

		htlc.AttemptTime = trunc(htlc.AttemptTime)
		if htlc.Settle != nil {
			htlc.Settle.SettleTime = trunc(
				htlc.Settle.SettleTime,
			)
		}
		if htlc.Failure != nil {
			htlc.Failure.FailTime = trunc(
				htlc.Failure.FailTime,
			)
		}

		// Clear cached fields not persisted in storage.
		htlc.onionBlob = [1366]byte{}
		htlc.circuit = nil
		htlc.cachedSessionKey = nil

		if len(htlc.Route.FirstHopWireCustomRecords) == 0 {
			htlc.Route.FirstHopWireCustomRecords = nil
		}

		for j := range htlc.Route.Hops {
			if len(htlc.Route.Hops[j].CustomRecords) == 0 {
				htlc.Route.Hops[j].CustomRecords = nil
			}
		}
	}
}

type duplicateRecord struct {
	PaymentIdentifier []byte
	AmountMsat        int64
	CreatedAt         time.Time
	FailReason        sql.NullInt32
	SettlePreimage    []byte
	SettleTime        sql.NullTime
}

// compareDuplicatePayments validates migrated duplicate rows against KV data.
func compareDuplicatePayments(ctx context.Context, paymentBucket kvdb.RBucket,
	sqlDB SQLQueries, paymentID int64, hash lntypes.Hash) error {

	kvDuplicates, err := fetchDuplicateRecords(paymentBucket)
	if err != nil {
		return fmt.Errorf("fetch KV duplicates %x: %w",
			hash[:8], err)
	}

	sqlDuplicates, err := sqlDB.FetchPaymentDuplicates(ctx, paymentID)
	if err != nil {
		return fmt.Errorf("fetch SQL duplicates %x: %w",
			hash[:8], err)
	}

	if len(kvDuplicates) != len(sqlDuplicates) {
		return fmt.Errorf("duplicate count mismatch %x: kv=%d "+
			"sql=%d", hash[:8], len(kvDuplicates),
			len(sqlDuplicates))
	}

	kvNormalized := normalizeDuplicateRecords(kvDuplicates)
	sqlNormalized := normalizeDuplicateRecords(
		convertSQLDuplicates(sqlDuplicates),
	)

	sortDuplicates(kvNormalized)
	sortDuplicates(sqlNormalized)

	if !reflect.DeepEqual(kvNormalized, sqlNormalized) {
		dumpCfg := spew.ConfigState{
			DisablePointerAddresses: true,
			DisableCapacities:       true,
			DisableMethods:          true,
			SortKeys:                true,
		}
		diff := difflib.UnifiedDiff{
			A: difflib.SplitLines(
				dumpCfg.Sdump(kvNormalized),
			),
			B: difflib.SplitLines(
				dumpCfg.Sdump(sqlNormalized),
			),
			FromFile: "kv",
			ToFile:   "sql",
			Context:  3,
		}
		diffText, _ := difflib.GetUnifiedDiffString(diff)

		return fmt.Errorf("duplicate mismatch %x\n%s",
			hash[:8], diffText)
	}

	return nil
}

// fetchDuplicateRecords reads duplicate payment records from the KV bucket.
func fetchDuplicateRecords(paymentBucket kvdb.RBucket) ([]duplicateRecord,
	error) {

	dupBucket := paymentBucket.NestedReadBucket(duplicatePaymentsBucket)
	if dupBucket == nil {
		return nil, nil
	}

	var duplicates []duplicateRecord
	err := dupBucket.ForEach(func(seqBytes, _ []byte) error {
		if len(seqBytes) != 8 {
			return nil
		}

		subBucket := dupBucket.NestedReadBucket(seqBytes)
		if subBucket == nil {
			return nil
		}

		creationData := subBucket.Get(duplicatePaymentCreationInfoKey)
		if creationData == nil {
			return fmt.Errorf("missing duplicate creation info")
		}

		creationInfo, err := deserializeDuplicatePaymentCreationInfo(
			bytes.NewReader(creationData),
		)
		if err != nil {
			return fmt.Errorf("deserialize duplicate creation "+
				"info: %w", err)
		}

		settleData := subBucket.Get(duplicatePaymentSettleInfoKey)
		failReasonData := subBucket.Get(duplicatePaymentFailInfoKey)

		if settleData != nil && len(failReasonData) > 0 {
			return fmt.Errorf("duplicate has both settle and " +
				"fail info")
		}

		var (
			failReason     sql.NullInt32
			settlePreimage []byte
			settleTime     sql.NullTime
		)

		switch {
		case settleData != nil:
			settlePreimage, settleTime, err =
				parseDuplicateSettleData(settleData)
			if err != nil {
				return err
			}
		case len(failReasonData) > 0:
			failReason = sql.NullInt32{
				Int32: int32(failReasonData[0]),
				Valid: true,
			}
		default:
			// If the duplicate has no settle or fail info, it is
			// considered failed. Every duplicate payment must have
			// either a settle or fail info in the sql database. So
			// we set the fail reason to error to mimic the behavior
			// for the kv store.
			failReason = sql.NullInt32{
				Int32: int32(FailureReasonError),
				Valid: true,
			}
		}

		duplicates = append(duplicates, duplicateRecord{
			PaymentIdentifier: creationInfo.PaymentIdentifier[:],
			AmountMsat:        int64(creationInfo.Value),
			CreatedAt: normalizeTimeForSQL(
				creationInfo.CreationTime,
			),
			FailReason:     failReason,
			SettlePreimage: settlePreimage,
			SettleTime:     settleTime,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return duplicates, nil
}

// convertSQLDuplicates maps SQL duplicate rows into comparable records.
func convertSQLDuplicates(rows []sqlc.PaymentDuplicate) []duplicateRecord {
	records := make([]duplicateRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, duplicateRecord{
			PaymentIdentifier: row.PaymentIdentifier,
			AmountMsat:        row.AmountMsat,
			CreatedAt:         row.CreatedAt,
			FailReason:        row.FailReason,
			SettlePreimage:    row.SettlePreimage,
			SettleTime:        row.SettleTime,
		})
	}

	return records
}

// normalizeDuplicateRecords normalizes time precision and empty fields.
func normalizeDuplicateRecords(records []duplicateRecord) []duplicateRecord {
	if len(records) == 0 {
		return []duplicateRecord{}
	}

	trunc := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}

		return t.In(time.Local).Truncate(time.Microsecond)
	}

	for i := range records {
		records[i].CreatedAt = trunc(records[i].CreatedAt)
		if records[i].SettleTime.Valid {
			records[i].SettleTime.Time = trunc(
				records[i].SettleTime.Time,
			)
		}
		if len(records[i].SettlePreimage) == 0 {
			records[i].SettlePreimage = nil
		}
	}

	return records
}

// sortDuplicates orders records deterministically for deep comparison.
func sortDuplicates(records []duplicateRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		ai := records[i]
		aj := records[j]

		if ai.CreatedAt.Equal(aj.CreatedAt) {
			if bytes.Equal(
				ai.PaymentIdentifier, aj.PaymentIdentifier,
			) {

				return ai.AmountMsat < aj.AmountMsat
			}

			return bytes.Compare(
				ai.PaymentIdentifier, aj.PaymentIdentifier,
			) < 0
		}

		return ai.CreatedAt.Before(aj.CreatedAt)
	})
}

// validatePaymentCounts compares the number of migrated payments with the SQL
// payment count to catch missing rows.
func validatePaymentCounts(ctx context.Context, sqlDB SQLQueries,
	expectedCount int64) error {

	sqlCount, err := sqlDB.CountPayments(ctx)
	if err != nil {
		return fmt.Errorf("count SQL payments: %w", err)
	}
	if expectedCount != sqlCount {
		return fmt.Errorf("payment count mismatch: kv=%d sql=%d",
			expectedCount, sqlCount)
	}

	return nil
}
