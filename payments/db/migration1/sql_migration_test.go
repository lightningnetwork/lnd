//go:build test_db_postgres || test_db_sqlite

package migration1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/payments/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestMigrationKVToSQL tests the basic payment migration from KV to SQL.
func TestMigrationKVToSQL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Setup KV database and populate with test data.
	kvDB := setupTestKVDB(t)
	populateTestPayments(t, kvDB, 5)

	sqlStore := setupTestSQLDB(t)

	// Run migration in a single transaction.
	err := sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(ctx, kvDB, tx,
				&SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)
}

// TestMigrationSequenceOrder ensures the migration follows sequence order
// rather than lexicographic hash order.
func TestMigrationSequenceOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	kvDB := setupTestKVDB(t)
	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		var globalAttemptID uint64
		hash0 := [32]byte{}
		hash1 := [32]byte{}
		hash2 := [32]byte{}
		hash0[0] = 3
		hash1[0] = 2
		hash2[0] = 1

		if err := createTestPaymentInKV(
			t, paymentsBucket, indexBucket, 0, hash0,
			&globalAttemptID,
		); err != nil {
			return err
		}

		// We make sure that the duplicate payment is skipped because
		// it will be migrated separately into payment_duplicates.
		if err := createTestDuplicatePaymentWithIndex(
			t, paymentsBucket, indexBucket, hash0, 1, false,
			&globalAttemptID,
		); err != nil {
			return err
		}
		if err := createTestPaymentInKV(
			t, paymentsBucket, indexBucket, 2, hash1,
			&globalAttemptID,
		); err != nil {
			return err
		}
		if err := createTestPaymentInKV(
			t, paymentsBucket, indexBucket, 3, hash2,
			&globalAttemptID,
		); err != nil {
			return err
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	resp, err := sqlStore.QueryPayments(ctx, Query{
		MaxPayments:       10,
		IncludeIncomplete: true,
	})
	require.NoError(t, err)
	require.Len(t, resp.Payments, 3)

	var (
		exp0 lntypes.Hash
		exp1 lntypes.Hash
		exp2 lntypes.Hash
	)
	exp0[0] = 3
	exp1[0] = 2
	exp2[0] = 1

	require.Equal(t, exp0, resp.Payments[0].Info.PaymentIdentifier)
	require.Equal(t, exp1, resp.Payments[1].Info.PaymentIdentifier)
	require.Equal(t, exp2, resp.Payments[2].Info.PaymentIdentifier)
}

// TestMigrationDataIntegrity verifies that migrated payment data exactly
// matches the original KV data when fetched through the SQLStore
// (SQLStore.FetchPayment). This covers the SQLStore query path separately
// from the migration's own batch validation.
func TestMigrationDataIntegrity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Setup KV database with test data.
	kvDB := setupTestKVDB(t)
	numPayments := populateTestPayments(t, kvDB, 5)

	// Fetch all payments from KV before migration.
	kvPayments := fetchAllPaymentsFromKV(t, kvDB)
	require.Len(t, kvPayments, numPayments)

	// Setup SQL database and run migration.
	sqlStore := setupTestSQLDB(t)

	err := sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	// Compare each KV payment with its SQL counterpart using deep equality.
	// This ensures that ALL fields match, not just a few selected ones.
	for _, kvPayment := range kvPayments {
		comparePaymentData(t, ctx, sqlStore, kvPayment)
	}
}

// TestMigrationWithDuplicates tests migration of duplicate payments into
// the payment_duplicates table.
func TestMigrationWithDuplicates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Setup KV database.
	kvDB := setupTestKVDB(t)

	// Create a payment with duplicates.
	hash := createTestPaymentHash(t, 0)
	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		// Create root buckets.
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		// Create primary payment with sequence 0 and globally unique
		// attempt ID.
		var globalAttemptID uint64
		err = createTestPaymentInKV(
			t, paymentsBucket, indexBucket, 0, hash,
			&globalAttemptID,
		)
		if err != nil {
			return err
		}

		// Add 2 duplicate payments for the same hash.
		paymentBucket := paymentsBucket.NestedReadWriteBucket(hash[:])
		require.NotNil(t, paymentBucket)

		dupBucket, err := paymentBucket.CreateBucketIfNotExists(
			duplicatePaymentsBucket,
		)
		if err != nil {
			return err
		}

		// Create duplicate with sequence 1 using global attempt ID.
		err = createTestDuplicatePayment(
			t, dupBucket, hash, 1, true, &globalAttemptID,
		)
		if err != nil {
			return err
		}

		// Create duplicate with sequence 2 using global attempt ID.
		err = createTestDuplicatePayment(
			t, dupBucket, hash, 2, false, &globalAttemptID,
		)
		if err != nil {
			return err
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	// Run migration.
	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	// Verify in SQL database.
	var count int64
	err = sqlStore.db.ExecTx(
		ctx, sqldb.ReadTxOpt(), func(q SQLQueries) error {
			var err error
			count, err = q.CountPayments(ctx)
			return err
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)
	require.Equal(
		t, int64(1), count, "SQL DB should have 1 payment",
	)

	var (
		dbPayment  sqlc.FetchPaymentRow
		duplicates []sqlc.PaymentDuplicate
	)
	err = sqlStore.db.ExecTx(
		ctx, sqldb.ReadTxOpt(), func(q SQLQueries) error {
			var err error
			dbPayment, err = q.FetchPayment(ctx, hash[:])
			if err != nil {
				return err
			}

			duplicates, err = q.FetchPaymentDuplicates(
				ctx, dbPayment.Payment.ID,
			)
			return err
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	require.Len(t, duplicates, 2)
	sort.SliceStable(duplicates, func(i, j int) bool {
		return duplicates[i].AmountMsat < duplicates[j].AmountMsat
	})

	require.Equal(t, hash[:], duplicates[0].PaymentIdentifier)
	require.Equal(t, int64(2001000), duplicates[0].AmountMsat)
	require.False(t, duplicates[0].FailReason.Valid)
	require.NotEmpty(t, duplicates[0].SettlePreimage)

	require.Equal(t, hash[:], duplicates[1].PaymentIdentifier)
	require.Equal(t, int64(2002000), duplicates[1].AmountMsat)
	require.True(t, duplicates[1].FailReason.Valid)
	require.Equal(
		t, int32(FailureReasonError),
		duplicates[1].FailReason.Int32,
	)
	require.Empty(t, duplicates[1].SettlePreimage)
}

// TestDuplicatePaymentsWithoutAttemptInfo verifies duplicate payments without
// attempt info are migrated with terminal failure reasons.
func TestDuplicatePaymentsWithoutAttemptInfo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	hash := createTestPaymentHash(t, 0)

	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		var globalAttemptID uint64
		err = createTestPaymentInKV(
			t, paymentsBucket, indexBucket, 1, hash,
			&globalAttemptID,
		)
		if err != nil {
			return err
		}

		paymentBucket := paymentsBucket.NestedReadWriteBucket(
			hash[:],
		)
		require.NotNil(t, paymentBucket)

		dupBucket, err := paymentBucket.CreateBucketIfNotExists(
			duplicatePaymentsBucket,
		)
		if err != nil {
			return err
		}

		if err := createDuplicateWithoutAttemptInfo(
			t, dupBucket, hash, 2, true, false,
		); err != nil {
			return err
		}
		if err := createDuplicateWithoutAttemptInfo(
			t, dupBucket, hash, 3, false, true,
		); err != nil {
			return err
		}
		if err := createDuplicateWithoutAttemptInfo(
			t, dupBucket, hash, 4, false, false,
		); err != nil {
			return err
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)
	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	var (
		dbPayment  sqlc.FetchPaymentRow
		duplicates []sqlc.PaymentDuplicate
	)
	err = sqlStore.db.ExecTx(
		ctx, sqldb.ReadTxOpt(), func(q SQLQueries) error {
			var err error
			dbPayment, err = q.FetchPayment(ctx, hash[:])
			if err != nil {
				return err
			}

			duplicates, err = q.FetchPaymentDuplicates(
				ctx, dbPayment.Payment.ID,
			)
			return err
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	require.Len(t, duplicates, 3)
	sort.SliceStable(duplicates, func(i, j int) bool {
		return duplicates[i].AmountMsat < duplicates[j].AmountMsat
	})

	require.Equal(t, int64(2002000), duplicates[0].AmountMsat)
	require.NotEmpty(t, duplicates[0].SettlePreimage)
	require.False(t, duplicates[0].FailReason.Valid)

	require.Equal(t, int64(2003000), duplicates[1].AmountMsat)
	require.True(t, duplicates[1].FailReason.Valid)
	require.Equal(
		t, int32(FailureReasonNoRoute),
		duplicates[1].FailReason.Int32,
	)
	require.Empty(t, duplicates[1].SettlePreimage)

	require.Equal(t, int64(2004000), duplicates[2].AmountMsat)
	require.True(t, duplicates[2].FailReason.Valid)
	require.Equal(
		t, int32(FailureReasonError),
		duplicates[2].FailReason.Int32,
	)
	require.Empty(t, duplicates[2].SettlePreimage)
}

// TestMigratePaymentWithMPP tests migration of a payment with MPP (multi-path
// payment) records.
func TestMigratePaymentWithMPP(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	// Create a payment with MPP.
	var paymentHash [32]byte
	copy(paymentHash[:], []byte("test_mpp_payment_hash_12345"))

	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		return createPaymentWithMPP(
			t, paymentsBucket, indexBucket, paymentHash,
		)
	}, func() {})
	require.NoError(t, err)

	// Run migration.
	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	// Verify payment matches.
	assertPaymentDataMatches(t, ctx, kvDB, sqlStore, paymentHash)
}

// TestMigratePaymentWithAMP tests migration of a payment with AMP (atomic
// multi-path) records.
func TestMigratePaymentWithAMP(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	var paymentHash [32]byte
	copy(paymentHash[:], []byte("test_amp_payment_hash_12345"))

	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		return createPaymentWithAMP(
			t, paymentsBucket, indexBucket, paymentHash,
		)
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	assertPaymentDataMatches(t, ctx, kvDB, sqlStore, paymentHash)
}

// TestMigratePaymentWithAMPSignedChildIndex tests migration of an AMP payment
// where the child index has the signed bit set.
func TestMigratePaymentWithAMPSignedChildIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	var paymentHash [32]byte
	copy(paymentHash[:], []byte("test_amp_child_idx_8000"))

	const childIndex = uint32(0x80000001)

	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		return createPaymentWithAMPChildIndex(
			t, paymentsBucket, indexBucket, paymentHash, childIndex,
		)
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	assertPaymentDataMatches(t, ctx, kvDB, sqlStore, paymentHash)
}

// TestMigratePaymentWithCustomRecords tests migration of a payment with custom
// records.
func TestMigratePaymentWithCustomRecords(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	var paymentHash [32]byte
	copy(paymentHash[:], []byte("test_custom_records_hash_12"))

	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		return createPaymentWithCustomRecords(
			t, paymentsBucket, indexBucket, paymentHash,
		)
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	assertPaymentDataMatches(t, ctx, kvDB, sqlStore, paymentHash)
}

// TestMigratePaymentWithBlindedRoute tests migration of a payment with blinded
// route.
func TestMigratePaymentWithBlindedRoute(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	var paymentHash [32]byte
	copy(paymentHash[:], []byte("test_blinded_route_hash_123"))

	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		return createPaymentWithBlindedRoute(
			t, paymentsBucket, indexBucket, paymentHash,
		)
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	assertPaymentDataMatches(t, ctx, kvDB, sqlStore, paymentHash)
}

// TestMigratePaymentWithMetadata tests migration of a payment with hop
// metadata.
func TestMigratePaymentWithMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	var paymentHash [32]byte
	copy(paymentHash[:], []byte("test_metadata_payment_hash_"))

	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		return createPaymentWithMetadata(
			t, paymentsBucket, indexBucket, paymentHash,
		)
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	assertPaymentDataMatches(t, ctx, kvDB, sqlStore, paymentHash)
}

// TestMigratePaymentWithAllFeatures tests migration with all optional
// features enabled.
func TestMigratePaymentWithAllFeatures(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	var paymentHash [32]byte
	copy(paymentHash[:], []byte("test_all_features_hash_1234"))

	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		return createPaymentWithAllFeatures(
			t, paymentsBucket, indexBucket, paymentHash,
		)
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	assertPaymentDataMatches(t, ctx, kvDB, sqlStore, paymentHash)
}

// TestMigratePaymentFeatureCombinations tests selected feature combinations
// in a single migration to cover interactions without random data.
func TestMigratePaymentFeatureCombinations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	cases := []paymentFeatureSet{
		{
			name:          "mpp_custom",
			mpp:           true,
			customRecords: true,
		},
		{
			name:         "amp_blinded",
			amp:          true,
			blindedRoute: true,
		},
		{
			name:          "custom_metadata",
			customRecords: true,
			hopMetadata:   true,
		},
		{
			name:         "blinded_metadata",
			blindedRoute: true,
			hopMetadata:  true,
		},
		{
			name:        "mpp_metadata",
			mpp:         true,
			hopMetadata: true,
		},
	}

	hashes := make([][32]byte, 0, len(cases))
	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		var globalAttemptID uint64
		for i, c := range cases {
			hash := sha256.Sum256([]byte(c.name))
			hashes = append(hashes, hash)

			err := createPaymentWithFeatureSet(
				t, paymentsBucket, indexBucket, hash,
				uint64(10+i), c, &globalAttemptID,
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	for _, hash := range hashes {
		assertPaymentDataMatches(t, ctx, kvDB, sqlStore, hash)
	}
}

// TestMigratePaymentWithFailureMessage tests migration of a payment with a
// failed HTLC that includes a failure message.
func TestMigratePaymentWithFailureMessage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kvDB := setupTestKVDB(t)

	var paymentHash [32]byte
	copy(paymentHash[:], []byte("test_fail_msg_hash_123456789"))

	// Create a payment with a failed HTLC
	err := kvdb.Update(kvDB, func(tx kvdb.RwTx) error {
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		// Create payment bucket
		paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(
			paymentHash[:],
		)
		if err != nil {
			return err
		}

		// Add creation info
		var paymentID lntypes.Hash
		copy(paymentID[:], paymentHash[:])

		creationInfo := &PaymentCreationInfo{
			PaymentIdentifier: paymentID,
			Value:             lnwire.MilliSatoshi(1000000),
			CreationTime:      time.Now().Add(-24 * time.Hour),
			PaymentRequest:    []byte("lnbc10utest"),
		}

		// Use a separate buffer for payment creation info to avoid
		// reuse issues when serializing HTLC attempts later.
		var creationInfoBuf bytes.Buffer
		err = serializePaymentCreationInfo(
			&creationInfoBuf, creationInfo,
		)
		if err != nil {
			return err
		}

		serialized := creationInfoBuf.Bytes()

		err = paymentBucket.Put(
			paymentCreationInfoKey, serialized,
		)
		if err != nil {
			return err
		}

		// Add sequence number
		seqBytes := make([]byte, 8)
		byteOrder.PutUint64(seqBytes, 50)
		err = paymentBucket.Put(paymentSequenceKey, seqBytes)
		if err != nil {
			return err
		}

		// Add payment-level failure reason
		failReasonBytes := []byte{byte(FailureReasonNoRoute)}
		err = paymentBucket.Put(
			paymentFailInfoKey, failReasonBytes,
		)
		if err != nil {
			return err
		}

		// Create HTLC bucket with one failed attempt
		htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
			paymentHtlcsBucket,
		)
		if err != nil {
			return err
		}

		// Create the failed attempt with a failure message
		attemptID := uint64(500)
		sessionKey, err := btcec.NewPrivateKey()
		if err != nil {
			return err
		}

		var sessionKeyBytes [32]byte
		copy(sessionKeyBytes[:], sessionKey.Serialize())

		var sourcePubKey route.Vertex
		copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

		hopKey, err := btcec.NewPrivateKey()
		if err != nil {
			return err
		}

		// Create a proper copy of the hash instead of referencing
		// the local variable directly.
		attemptHash := new(lntypes.Hash)
		copy(attemptHash[:], paymentHash[:])

		//nolint:ll
		attemptInfo := &HTLCAttemptInfo{
			AttemptID:  attemptID,
			sessionKey: sessionKeyBytes,
			Route: route.Route{
				TotalTimeLock: 500000,
				TotalAmount:   900,
				SourcePubKey:  sourcePubKey,
				Hops: []*route.Hop{
					{
						PubKeyBytes:      route.NewVertex(hopKey.PubKey()),
						ChannelID:        12345,
						OutgoingTimeLock: 499500,
						AmtToForward:     850,
					},
				},
			},
			AttemptTime: time.Now().Add(-2 * time.Hour),
			Hash:        attemptHash,
		}

		// Write attempt info
		attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
		copy(attemptKey, htlcAttemptInfoKey)
		byteOrder.PutUint64(
			attemptKey[len(htlcAttemptInfoKey):], attemptID,
		)

		var b bytes.Buffer
		err = serializeHTLCAttemptInfo(&b, attemptInfo)
		if err != nil {
			return err
		}
		err = htlcBucket.Put(attemptKey, b.Bytes())
		if err != nil {
			return err
		}

		// Add failure info with a message
		//nolint:ll
		failInfo := &HTLCFailInfo{
			FailTime:           time.Now().Add(-1 * time.Hour),
			Message:            &lnwire.FailTemporaryChannelFailure{},
			Reason:             HTLCFailMessage,
			FailureSourceIndex: 1,
		}

		failKey := make([]byte, len(htlcFailInfoKey)+8)
		copy(failKey, htlcFailInfoKey)
		byteOrder.PutUint64(failKey[len(htlcFailInfoKey):], attemptID)

		b.Reset()
		if err := serializeHTLCFailInfo(&b, failInfo); err != nil {
			return err
		}
		if err := htlcBucket.Put(failKey, b.Bytes()); err != nil {
			return err
		}

		// Create index entry.
		var idx bytes.Buffer
		if err := WriteElements(
			&idx, paymentIndexTypeHash, paymentHash[:],
		); err != nil {
			return err
		}
		return indexBucket.Put(seqBytes, idx.Bytes())
	}, func() {})
	require.NoError(t, err)

	// Migrate to SQL
	sqlStore := setupTestSQLDB(t)

	err = sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
			return MigratePaymentsKVToSQL(
				ctx, kvDB, tx, &SQLStoreConfig{
					QueryCfg: sqlStore.cfg.QueryCfg,
				},
			)
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	// Verify data matches.
	assertPaymentDataMatches(t, ctx, kvDB, sqlStore, paymentHash)
}

// setupTestKVDB creates a temporary KV database for testing.
func setupTestKVDB(t *testing.T) kvdb.Backend {
	t.Helper()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "payments")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	return backend
}

// populateTestPayments populates the KV database with test payment data.
func populateTestPayments(t *testing.T, db kvdb.Backend, numPayments int) int {
	t.Helper()

	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		// Create root buckets.
		paymentsBucket, err := tx.CreateTopLevelBucket(
			paymentsRootBucket,
		)
		if err != nil {
			return err
		}

		indexBucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
		if err != nil {
			return err
		}

		// Create test payments with globally unique attempt IDs.
		var globalAttemptID uint64
		for i := 0; i < numPayments; i++ {
			hash := createTestPaymentHash(t, i)

			err := createTestPaymentInKV(
				t, paymentsBucket, indexBucket, uint64(i), hash,
				&globalAttemptID,
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})

	require.NoError(t, err)
	return numPayments
}

// serializeDuplicatePaymentCreationInfo serializes PaymentCreationInfo for
// duplicate payments. The time is stored in seconds (not nanoseconds) to match
// the format used by deserializeDuplicatePaymentCreationInfo in the KV store.
func serializeDuplicatePaymentCreationInfo(w io.Writer,
	c *PaymentCreationInfo) error {

	var scratch [8]byte

	if _, err := w.Write(c.PaymentIdentifier[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(c.Value))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	// Store time in seconds (not nanoseconds) for duplicate payments.
	// This matches the deserialization format used in
	// deserializeDuplicatePaymentCreationInfo.
	var unixSec int64
	if !c.CreationTime.IsZero() {
		unixSec = c.CreationTime.Unix()
	}
	byteOrder.PutUint64(scratch[:], uint64(unixSec))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], uint32(len(c.PaymentRequest)))
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	if _, err := w.Write(c.PaymentRequest); err != nil {
		return err
	}

	return nil
}

// createTestPaymentHash creates a deterministic payment hash for testing.
func createTestPaymentHash(t *testing.T, seed int) [32]byte {
	t.Helper()

	hash := sha256.Sum256([]byte{byte(seed)})
	return hash
}

// createTestPaymentInKV creates a single payment in the KV store.
func createTestPaymentInKV(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, seqNum uint64, hash [32]byte,
	globalAttemptID *uint64) error {

	t.Helper()

	// Create payment bucket.
	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(hash[:])
	if err != nil {
		return err
	}

	// Create payment creation info.
	var paymentID lntypes.Hash
	copy(paymentID[:], hash[:])

	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(1000000 + seqNum*1000),
		CreationTime:      time.Now().Add(-24 * time.Hour),
		PaymentRequest:    []byte("lnbc1test"),
	}

	// Serialize and write creation info.
	var b bytes.Buffer
	err = serializePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = paymentBucket.Put(paymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	// Store sequence number.
	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, seqNum)
	err = paymentBucket.Put(paymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	// Add one HTLC attempt for each payment with globally unique ID.
	htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
		paymentHtlcsBucket,
	)
	if err != nil {
		return err
	}

	// Increment global attempt ID and create HTLC attempt. So we have a
	// globally unique attempt ID for the HTLC attempt.
	*globalAttemptID++
	err = createTestHTLCAttempt(
		t, htlcBucket, hash, *globalAttemptID, seqNum%3 == 0,
	)
	if err != nil {
		return err
	}

	var idx bytes.Buffer
	err = WriteElements(&idx, paymentIndexTypeHash, hash[:])
	if err != nil {
		return err
	}

	return indexBucket.Put(seqBytes, idx.Bytes())
}

// createTestHTLCAttempt creates a test HTLC attempt in the KV store.
func createTestHTLCAttempt(t *testing.T, htlcBucket kvdb.RwBucket,
	paymentHash [32]byte, attemptID uint64, shouldSettle bool) error {
	t.Helper()

	// Generate a session key.
	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create a simple 2-hop route.
	hop1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hop2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	// Convert session key to [32]byte.
	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	attemptInfo := &HTLCAttemptInfo{
		AttemptID:  attemptID,
		sessionKey: sessionKeyBytes,
		Route: route.Route{
			TotalTimeLock: 500000,
			TotalAmount:   900,
			SourcePubKey:  sourcePubKey,
			Hops: []*route.Hop{
				{
					PubKeyBytes: route.NewVertex(
						hop1Key.PubKey(),
					),
					ChannelID:        12345,
					OutgoingTimeLock: 499500,
					AmtToForward:     850,
				},
				{
					PubKeyBytes: route.NewVertex(
						hop2Key.PubKey(),
					),
					ChannelID:        67890,
					OutgoingTimeLock: 499000,
					AmtToForward:     800,
				},
			},
		},
		AttemptTime: time.Now().Add(-2 * time.Hour),
		Hash:        (*lntypes.Hash)(&paymentHash),
	}

	// Serialize and write attempt info.
	attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
	copy(attemptKey, htlcAttemptInfoKey)
	byteOrder.PutUint64(attemptKey[len(htlcAttemptInfoKey):], attemptID)

	var b bytes.Buffer
	err = serializeHTLCAttemptInfo(&b, attemptInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(attemptKey, b.Bytes())
	if err != nil {
		return err
	}

	// Add settlement if requested.
	if shouldSettle {
		settleInfo := &HTLCSettleInfo{
			Preimage:   lntypes.Preimage(paymentHash),
			SettleTime: time.Now().Add(-1 * time.Hour),
		}

		settleKey := make([]byte, len(htlcSettleInfoKey)+8)
		copy(settleKey, htlcSettleInfoKey)
		byteOrder.PutUint64(
			settleKey[len(htlcSettleInfoKey):], attemptID,
		)

		var sb bytes.Buffer
		err = serializeHTLCSettleInfo(&sb, settleInfo)
		if err != nil {
			return err
		}
		err = htlcBucket.Put(settleKey, sb.Bytes())
		if err != nil {
			return err
		}
	}

	return nil
}

// createTestDuplicatePaymentWithIndex creates a duplicate payment and adds
// a matching entry into the global payment sequence index.
func createTestDuplicatePaymentWithIndex(t *testing.T,
	paymentsBucket kvdb.RwBucket, indexBucket kvdb.RwBucket,
	paymentHash [32]byte, seqNum uint64, shouldSettle bool,
	globalAttemptID *uint64) error {
	t.Helper()

	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(
		paymentHash[:],
	)
	if err != nil {
		return err
	}

	dupBucket, err := paymentBucket.CreateBucketIfNotExists(
		duplicatePaymentsBucket,
	)
	if err != nil {
		return err
	}

	if err := createTestDuplicatePayment(
		t, dupBucket, paymentHash, seqNum, shouldSettle,
		globalAttemptID,
	); err != nil {
		return err
	}

	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, seqNum)
	var idx bytes.Buffer
	if err := WriteElements(
		&idx, paymentIndexTypeHash, paymentHash[:],
	); err != nil {
		return err
	}

	return indexBucket.Put(seqBytes, idx.Bytes())
}

// createTestDuplicatePayment creates a duplicate payment in the KV store.
func createTestDuplicatePayment(t *testing.T,
	dupBucket kvdb.RwBucket, paymentHash [32]byte, seqNum uint64,
	shouldSettle bool, globalAttemptID *uint64) error {

	t.Helper()

	// Create bucket for this duplicate using sequence number as key.
	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, seqNum)

	dupPaymentBucket, err := dupBucket.CreateBucketIfNotExists(seqBytes)
	if err != nil {
		return err
	}

	// Store sequence number.
	err = dupPaymentBucket.Put(duplicatePaymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	// Create payment creation info.
	var paymentID lntypes.Hash
	copy(paymentID[:], paymentHash[:])

	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(2000000 + seqNum*1000),
		CreationTime:      time.Now().Add(-48 * time.Hour),
		PaymentRequest:    []byte("lnbc1duplicate"),
	}

	var b bytes.Buffer
	err = serializeDuplicatePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = dupPaymentBucket.Put(duplicatePaymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	// Generate a session key for the duplicate attempt.
	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create route for duplicate.
	hop1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hop2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	// Use globally unique attempt ID.
	*globalAttemptID++
	attemptID := *globalAttemptID

	duplicateAttempt := &duplicateHTLCAttemptInfo{
		attemptID:  attemptID,
		sessionKey: sessionKeyBytes,
		route: route.Route{
			TotalTimeLock: 500000,
			TotalAmount:   900,
			SourcePubKey:  sourcePubKey,
			Hops: []*route.Hop{
				{
					PubKeyBytes: route.NewVertex(
						hop1Key.PubKey(),
					),
					ChannelID:        12345,
					OutgoingTimeLock: 499500,
					AmtToForward:     850,
				},
				{
					PubKeyBytes: route.NewVertex(
						hop2Key.PubKey(),
					),
					ChannelID:        67890,
					OutgoingTimeLock: 499000,
					AmtToForward:     800,
				},
			},
		},
	}

	// Serialize and write attempt info (using existing WriteElements
	// and SerializeRoute).
	var ab bytes.Buffer
	if err := WriteElements(
		&ab, duplicateAttempt.attemptID,
		duplicateAttempt.sessionKey,
	); err != nil {
		return err
	}
	if err := SerializeRoute(&ab, duplicateAttempt.route); err != nil {
		return err
	}
	err = dupPaymentBucket.Put(duplicatePaymentAttemptInfoKey, ab.Bytes())
	if err != nil {
		return err
	}

	// Add settlement if requested.
	if shouldSettle {
		settleInfo := &HTLCSettleInfo{
			Preimage:   lntypes.Preimage(paymentHash),
			SettleTime: time.Now().Add(-1 * time.Hour),
		}

		var sb bytes.Buffer
		err = serializeHTLCSettleInfo(&sb, settleInfo)
		if err != nil {
			return err
		}
		err = dupPaymentBucket.Put(
			duplicatePaymentSettleInfoKey, sb.Bytes(),
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// createDuplicateWithoutAttemptInfo creates a duplicate payment bucket with
// settle/fail info but without attempt info.
func createDuplicateWithoutAttemptInfo(t *testing.T,
	dupBucket kvdb.RwBucket, paymentHash [32]byte, seqNum uint64,
	shouldSettle bool, shouldFail bool) error {
	t.Helper()

	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, seqNum)

	dupPaymentBucket, err := dupBucket.CreateBucketIfNotExists(seqBytes)
	if err != nil {
		return err
	}

	if err := dupPaymentBucket.Put(
		duplicatePaymentSequenceKey, seqBytes,
	); err != nil {
		return err
	}

	var paymentID lntypes.Hash
	copy(paymentID[:], paymentHash[:])

	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(2000000 + seqNum*1000),
		CreationTime:      time.Now().Add(-48 * time.Hour),
		PaymentRequest:    []byte("lnbc1duplicate"),
	}

	var b bytes.Buffer
	err = serializeDuplicatePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	if err := dupPaymentBucket.Put(
		duplicatePaymentCreationInfoKey, b.Bytes(),
	); err != nil {
		return err
	}

	switch {
	case shouldSettle && shouldFail:
		return fmt.Errorf("invalid duplicate state")
	case shouldSettle:
		settleInfo := &HTLCSettleInfo{
			Preimage:   lntypes.Preimage(paymentHash),
			SettleTime: time.Now().Add(-1 * time.Hour),
		}

		var sb bytes.Buffer
		err = serializeHTLCSettleInfo(&sb, settleInfo)
		if err != nil {
			return err
		}
		if err := dupPaymentBucket.Put(
			duplicatePaymentSettleInfoKey, sb.Bytes(),
		); err != nil {
			return err
		}
	case shouldFail:
		failReasonBytes := []byte{byte(FailureReasonNoRoute)}
		if err := dupPaymentBucket.Put(
			duplicatePaymentFailInfoKey, failReasonBytes,
		); err != nil {
			return err
		}
	}

	return nil
}

// fetchAllPaymentsFromKV fetches all payments from the KV store using the
// KVStore implementation.
func fetchAllPaymentsFromKV(t *testing.T, kvDB kvdb.Backend) []*MPPayment {
	t.Helper()

	kvStore, err := NewKVStore(kvDB, WithNoMigration(true))
	require.NoError(t, err)

	payments, err := kvStore.FetchPayments()
	require.NoError(t, err)

	return payments
}

// normalizePaymentData makes sure that the payment data is normalized for
// comparison. We align to local time and truncate to microsecond precision
// (matching invoice migration behavior).
func normalizePaymentData(payment *MPPayment) {
	trunc := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		return t.In(time.Local).Truncate(time.Microsecond)
	}

	// SequenceNum is a database ID which will differ between KV and SQL.
	payment.SequenceNum = 0

	// Normalize payment creation time.
	payment.Info.CreationTime = trunc(payment.Info.CreationTime)

	// Normalize payment-level custom records.
	if len(payment.Info.FirstHopCustomRecords) == 0 {
		payment.Info.FirstHopCustomRecords = nil
	}

	// Normalize HTLC attempt times.
	// Normalize nil vs empty slice for HTLCs.
	// SQL returns empty slice for payments with no attempts,
	// while KV returns nil.
	if payment.HTLCs == nil {
		payment.HTLCs = []HTLCAttempt{}
		return
	}

	for i := range payment.HTLCs {
		payment.HTLCs[i].AttemptTime = trunc(
			payment.HTLCs[i].AttemptTime,
		)

		if payment.HTLCs[i].Settle != nil {
			payment.HTLCs[i].Settle.SettleTime = trunc(
				payment.HTLCs[i].Settle.SettleTime,
			)
		}

		if payment.HTLCs[i].Failure != nil {
			payment.HTLCs[i].Failure.FailTime = trunc(
				payment.HTLCs[i].Failure.FailTime,
			)
		}

		// Zero out non-serialized cached fields (onionBlob and
		// circuit). These are computed on-demand and not stored in the
		// database.
		payment.HTLCs[i].onionBlob = [1366]byte{}
		payment.HTLCs[i].circuit = nil
		payment.HTLCs[i].cachedSessionKey = nil

		// Normalize route-level custom records.
		if len(payment.HTLCs[i].Route.FirstHopWireCustomRecords) == 0 {
			payment.HTLCs[i].Route.FirstHopWireCustomRecords = nil
		}

		// Normalize hop-level custom records.
		hops := payment.HTLCs[i].Route.Hops
		for j := range hops {
			if len(hops[j].CustomRecords) == 0 {
				hops[j].CustomRecords = nil
			}
		}
	}
}

// comparePaymentData compares a KV payment with its SQL counterpart using
// deep equality check (similar to invoice migration).
func comparePaymentData(t *testing.T, ctx context.Context, sqlStore *SQLStore,
	kvPayment *MPPayment) {

	t.Helper()

	// Fetch the SQL payment as MPPayment using SQLStore.
	var paymentHash lntypes.Hash
	copy(paymentHash[:], kvPayment.Info.PaymentIdentifier[:])

	sqlPayment, err := sqlStore.FetchPayment(ctx, paymentHash)
	require.NoError(t, err, "SQL payment should exist for %x",
		paymentHash[:8])

	// Normalize time precision to microseconds.
	normalizePaymentData(kvPayment)
	normalizePaymentData(sqlPayment)

	// Deep equality check - compares all fields recursively.
	require.Equal(t, kvPayment, sqlPayment,
		"KV and SQL payments should be equal for %x", paymentHash[:8])
}

// assertPaymentDataMatches verifies a payment in KV matches its SQL counterpart
// using deep equality check.
func assertPaymentDataMatches(t *testing.T, ctx context.Context,
	kvDB kvdb.Backend, sqlStore *SQLStore, hash [32]byte) {
	t.Helper()

	// Fetch from KV.
	var kvPayment *MPPayment
	err := kvdb.View(kvDB, func(tx kvdb.RTx) error {
		paymentsBucket := tx.ReadBucket(paymentsRootBucket)
		if paymentsBucket == nil {
			return nil
		}

		paymentBucket := paymentsBucket.NestedReadBucket(hash[:])
		if paymentBucket == nil {
			return nil
		}

		var err error
		kvPayment, err = fetchPayment(paymentBucket)
		return err
	}, func() {})
	require.NoError(t, err)

	if kvPayment == nil {
		// Payment doesn't exist in KV, should not exist in SQL
		// either.
		var paymentHash lntypes.Hash
		copy(paymentHash[:], hash[:])
		_, err := sqlStore.FetchPayment(ctx, paymentHash)
		require.Error(
			t, err, "payment should not exist in SQL if not "+
				"in KV",
		)
		return
	}

	// Use the deep comparison function.
	comparePaymentData(t, ctx, sqlStore, kvPayment)
}

// createPaymentWithMPP creates a payment with MPP records on the final hop.
func createPaymentWithMPP(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, hash [32]byte) error {
	t.Helper()

	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(hash[:])
	if err != nil {
		return err
	}

	// Create payment info.
	var paymentID lntypes.Hash
	copy(paymentID[:], hash[:])

	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(50000),
		CreationTime:      time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
		PaymentRequest:    []byte("lnbc500n1test_mpp"),
	}

	var b bytes.Buffer
	err = serializePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = paymentBucket.Put(paymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	// Store sequence number.
	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, 1)
	err = paymentBucket.Put(paymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	// Create HTLC with MPP.
	htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
		paymentHtlcsBucket,
	)
	if err != nil {
		return err
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	// Create route with 3 hops, MPP on final hop.
	hops := make([]*route.Hop, 3)
	for i := 0; i < 3; i++ {
		hopKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		amt := lnwire.MilliSatoshi(50000 - uint64(i)*100)
		hop := &route.Hop{
			PubKeyBytes:      route.NewVertex(hopKey.PubKey()),
			ChannelID:        uint64(100000 + i),
			OutgoingTimeLock: uint32(500000 - i*40),
			AmtToForward:     amt,
		}

		// Add MPP to final hop.
		if i == 2 {
			var paymentAddr [32]byte
			copy(
				paymentAddr[:],
				[]byte("test_mpp_payment_address_32"),
			)
			hop.MPP = record.NewMPP(
				lnwire.MilliSatoshi(50000),
				paymentAddr,
			)
		}

		hops[i] = hop
	}

	attemptInfo := &HTLCAttemptInfo{
		AttemptID:  1,
		sessionKey: sessionKeyBytes,
		Route: route.Route{
			TotalTimeLock: 500000,
			TotalAmount:   lnwire.MilliSatoshi(49800),
			SourcePubKey:  sourcePubKey,
			Hops:          hops,
		},
		AttemptTime: time.Date(2024, 1, 1, 12, 1, 0, 0, time.UTC),
		Hash:        (*lntypes.Hash)(&hash),
	}

	attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
	copy(attemptKey, htlcAttemptInfoKey)
	byteOrder.PutUint64(attemptKey[len(htlcAttemptInfoKey):], 1)

	var ab bytes.Buffer
	err = serializeHTLCAttemptInfo(&ab, attemptInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(attemptKey, ab.Bytes())
	if err != nil {
		return err
	}

	// Add settlement.
	settleInfo := &HTLCSettleInfo{
		Preimage:   lntypes.Preimage(hash),
		SettleTime: time.Date(2024, 1, 1, 12, 2, 0, 0, time.UTC),
	}

	settleKey := make([]byte, len(htlcSettleInfoKey)+8)
	copy(settleKey, htlcSettleInfoKey)
	byteOrder.PutUint64(settleKey[len(htlcSettleInfoKey):], 1)

	var sb bytes.Buffer
	err = serializeHTLCSettleInfo(&sb, settleInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(settleKey, sb.Bytes())
	if err != nil {
		return err
	}

	// Create index entry.
	var idx bytes.Buffer
	err = WriteElements(&idx, paymentIndexTypeHash, hash[:])
	if err != nil {
		return err
	}
	return indexBucket.Put(seqBytes, idx.Bytes())
}

// createPaymentWithAMP creates a payment with AMP records on the final hop.
func createPaymentWithAMP(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, hash [32]byte) error {
	t.Helper()

	return createPaymentWithAMPChildIndex(
		t, paymentsBucket, indexBucket, hash, 0,
	)
}

// createPaymentWithAMPChildIndex creates a payment with AMP records on the
// final hop and a specific child index.
func createPaymentWithAMPChildIndex(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, hash [32]byte, childIndex uint32) error {
	t.Helper()

	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(hash[:])
	if err != nil {
		return err
	}

	var paymentID lntypes.Hash
	copy(paymentID[:], hash[:])

	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(75000),
		CreationTime:      time.Date(2024, 2, 1, 10, 0, 0, 0, time.UTC),
		PaymentRequest:    []byte("lnbc750n1test_amp"),
	}

	var b bytes.Buffer
	err = serializePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = paymentBucket.Put(paymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, 2)
	err = paymentBucket.Put(paymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
		paymentHtlcsBucket,
	)
	if err != nil {
		return err
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	// Create route with AMP on final hop.
	hops := make([]*route.Hop, 2)
	for i := 0; i < 2; i++ {
		hopKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		amt := lnwire.MilliSatoshi(75000 - uint64(i)*50)
		hop := &route.Hop{
			PubKeyBytes:      route.NewVertex(hopKey.PubKey()),
			ChannelID:        uint64(200000 + i),
			OutgoingTimeLock: uint32(600000 - i*40),
			AmtToForward:     amt,
		}

		// Add AMP to final hop.
		if i == 1 {
			var rootShare [32]byte
			copy(
				rootShare[:],
				[]byte("test_amp_root_share_12345678"),
			)
			var setID [32]byte
			copy(setID[:], []byte("test_amp_set_id_123456789012"))
			hop.AMP = record.NewAMP(rootShare, setID, childIndex)
		}

		hops[i] = hop
	}

	attemptInfo := &HTLCAttemptInfo{
		AttemptID:  1,
		sessionKey: sessionKeyBytes,
		Route: route.Route{
			TotalTimeLock: 600000,
			TotalAmount:   lnwire.MilliSatoshi(74950),
			SourcePubKey:  sourcePubKey,
			Hops:          hops,
		},
		AttemptTime: time.Date(2024, 2, 1, 10, 1, 0, 0, time.UTC),
		Hash:        (*lntypes.Hash)(&hash),
	}

	attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
	copy(attemptKey, htlcAttemptInfoKey)
	byteOrder.PutUint64(attemptKey[len(htlcAttemptInfoKey):], 1)

	var ab bytes.Buffer
	err = serializeHTLCAttemptInfo(&ab, attemptInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(attemptKey, ab.Bytes())
	if err != nil {
		return err
	}

	// Add settlement.
	settleInfo := &HTLCSettleInfo{
		Preimage:   lntypes.Preimage(hash),
		SettleTime: time.Date(2024, 2, 1, 10, 2, 0, 0, time.UTC),
	}

	settleKey := make([]byte, len(htlcSettleInfoKey)+8)
	copy(settleKey, htlcSettleInfoKey)
	byteOrder.PutUint64(settleKey[len(htlcSettleInfoKey):], 1)

	var sb bytes.Buffer
	err = serializeHTLCSettleInfo(&sb, settleInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(settleKey, sb.Bytes())
	if err != nil {
		return err
	}

	var idx bytes.Buffer
	err = WriteElements(&idx, paymentIndexTypeHash, hash[:])
	if err != nil {
		return err
	}
	return indexBucket.Put(seqBytes, idx.Bytes())
}

// createPaymentWithCustomRecords creates a payment with custom records at all
// levels.
func createPaymentWithCustomRecords(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, hash [32]byte) error {
	t.Helper()

	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(hash[:])
	if err != nil {
		return err
	}

	var paymentID lntypes.Hash
	copy(paymentID[:], hash[:])

	// Payment-level custom records.
	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(100000),
		CreationTime:      time.Date(2024, 3, 1, 14, 0, 0, 0, time.UTC),
		PaymentRequest:    []byte("lnbc1m1test_custom"),
		FirstHopCustomRecords: lnwire.CustomRecords{
			65536: []byte("payment_level_value_1"),
			65537: []byte("payment_level_value_2"),
		},
	}

	var b bytes.Buffer
	err = serializePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = paymentBucket.Put(paymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, 3)
	err = paymentBucket.Put(paymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
		paymentHtlcsBucket,
	)
	if err != nil {
		return err
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	// Create route with custom records at all levels.
	hops := make([]*route.Hop, 3)
	for i := 0; i < 3; i++ {
		hopKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		amt := lnwire.MilliSatoshi(100000 - uint64(i)*150)
		hop := &route.Hop{
			PubKeyBytes:      route.NewVertex(hopKey.PubKey()),
			ChannelID:        uint64(300000 + i),
			OutgoingTimeLock: uint32(700000 - i*40),
			AmtToForward:     amt,
			// Hop-level custom records.
			CustomRecords: record.CustomSet{
				65538 + uint64(i): []byte(
					fmt.Sprintf("hop_%d_custom_value", i),
				),
			},
		}

		hops[i] = hop
	}

	attemptInfo := &HTLCAttemptInfo{
		AttemptID:  1,
		sessionKey: sessionKeyBytes,
		Route: route.Route{
			TotalTimeLock: 700000,
			TotalAmount:   lnwire.MilliSatoshi(99700),
			SourcePubKey:  sourcePubKey,
			Hops:          hops,
			// Attempt-level first hop custom records.
			FirstHopWireCustomRecords: lnwire.CustomRecords{
				65541: []byte("attempt_custom_value_1"),
				65542: []byte("attempt_custom_value_2"),
			},
		},
		AttemptTime: time.Date(2024, 3, 1, 14, 1, 0, 0, time.UTC),
		Hash:        (*lntypes.Hash)(&hash),
	}

	attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
	copy(attemptKey, htlcAttemptInfoKey)
	byteOrder.PutUint64(attemptKey[len(htlcAttemptInfoKey):], 1)

	var ab bytes.Buffer
	err = serializeHTLCAttemptInfo(&ab, attemptInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(attemptKey, ab.Bytes())
	if err != nil {
		return err
	}

	settleInfo := &HTLCSettleInfo{
		Preimage:   lntypes.Preimage(hash),
		SettleTime: time.Date(2024, 3, 1, 14, 2, 0, 0, time.UTC),
	}

	settleKey := make([]byte, len(htlcSettleInfoKey)+8)
	copy(settleKey, htlcSettleInfoKey)
	byteOrder.PutUint64(settleKey[len(htlcSettleInfoKey):], 1)

	var sb bytes.Buffer
	err = serializeHTLCSettleInfo(&sb, settleInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(settleKey, sb.Bytes())
	if err != nil {
		return err
	}

	var idx bytes.Buffer
	err = WriteElements(&idx, paymentIndexTypeHash, hash[:])
	if err != nil {
		return err
	}
	return indexBucket.Put(seqBytes, idx.Bytes())
}

// createPaymentWithBlindedRoute creates a payment with blinded route data.
func createPaymentWithBlindedRoute(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, hash [32]byte) error {
	t.Helper()

	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(hash[:])
	if err != nil {
		return err
	}

	var paymentID lntypes.Hash
	copy(paymentID[:], hash[:])

	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(120000),
		CreationTime:      time.Date(2024, 4, 1, 16, 0, 0, 0, time.UTC),
		PaymentRequest:    []byte("lnbc1200n1test_blinded"),
	}

	var b bytes.Buffer
	err = serializePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = paymentBucket.Put(paymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, 4)
	err = paymentBucket.Put(paymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
		paymentHtlcsBucket,
	)
	if err != nil {
		return err
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	// Create route with blinded data on final hop.
	hops := make([]*route.Hop, 4)
	for i := 0; i < 4; i++ {
		hopKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		amt := lnwire.MilliSatoshi(120000 - uint64(i)*200)
		hop := &route.Hop{
			PubKeyBytes:      route.NewVertex(hopKey.PubKey()),
			ChannelID:        uint64(400000 + i),
			OutgoingTimeLock: uint32(800000 - i*40),
			AmtToForward:     amt,
		}

		// Add blinded route data to final hop.
		if i == 3 {
			blindingKey, err := btcec.NewPrivateKey()
			require.NoError(t, err)

			hop.BlindingPoint = blindingKey.PubKey()
			hop.EncryptedData = []byte(
				"encrypted_blinded_route_data_test_value_12345",
			)
			hop.TotalAmtMsat = lnwire.MilliSatoshi(119400)
		}

		hops[i] = hop
	}

	attemptInfo := &HTLCAttemptInfo{
		AttemptID:  1,
		sessionKey: sessionKeyBytes,
		Route: route.Route{
			TotalTimeLock: 800000,
			TotalAmount:   lnwire.MilliSatoshi(119400),
			SourcePubKey:  sourcePubKey,
			Hops:          hops,
		},
		AttemptTime: time.Date(2024, 4, 1, 16, 1, 0, 0, time.UTC),
		Hash:        (*lntypes.Hash)(&hash),
	}

	attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
	copy(attemptKey, htlcAttemptInfoKey)
	byteOrder.PutUint64(attemptKey[len(htlcAttemptInfoKey):], 1)

	var ab bytes.Buffer
	err = serializeHTLCAttemptInfo(&ab, attemptInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(attemptKey, ab.Bytes())
	if err != nil {
		return err
	}

	settleInfo := &HTLCSettleInfo{
		Preimage:   lntypes.Preimage(hash),
		SettleTime: time.Date(2024, 4, 1, 16, 2, 0, 0, time.UTC),
	}

	settleKey := make([]byte, len(htlcSettleInfoKey)+8)
	copy(settleKey, htlcSettleInfoKey)
	byteOrder.PutUint64(settleKey[len(htlcSettleInfoKey):], 1)

	var sb bytes.Buffer
	err = serializeHTLCSettleInfo(&sb, settleInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(settleKey, sb.Bytes())
	if err != nil {
		return err
	}

	var idx bytes.Buffer
	err = WriteElements(&idx, paymentIndexTypeHash, hash[:])
	if err != nil {
		return err
	}
	return indexBucket.Put(seqBytes, idx.Bytes())
}

// createPaymentWithMetadata creates a payment with hop metadata.
func createPaymentWithMetadata(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, hash [32]byte) error {
	t.Helper()

	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(hash[:])
	if err != nil {
		return err
	}

	var paymentID lntypes.Hash
	copy(paymentID[:], hash[:])

	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(80000),
		CreationTime:      time.Date(2024, 5, 1, 18, 0, 0, 0, time.UTC),
		PaymentRequest:    []byte("lnbc800n1test_metadata"),
	}

	var b bytes.Buffer
	err = serializePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = paymentBucket.Put(paymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, 5)
	err = paymentBucket.Put(paymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
		paymentHtlcsBucket,
	)
	if err != nil {
		return err
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	// Create route with metadata on all hops.
	hops := make([]*route.Hop, 3)
	for i := 0; i < 3; i++ {
		hopKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		amt := lnwire.MilliSatoshi(80000 - uint64(i)*100)
		hop := &route.Hop{
			PubKeyBytes:      route.NewVertex(hopKey.PubKey()),
			ChannelID:        uint64(500000 + i),
			OutgoingTimeLock: uint32(900000 - i*40),
			AmtToForward:     amt,
			Metadata: []byte(
				fmt.Sprintf("hop_%d_metadata_value", i),
			),
		}

		hops[i] = hop
	}

	attemptInfo := &HTLCAttemptInfo{
		AttemptID:  1,
		sessionKey: sessionKeyBytes,
		Route: route.Route{
			TotalTimeLock: 900000,
			TotalAmount:   lnwire.MilliSatoshi(79800),
			SourcePubKey:  sourcePubKey,
			Hops:          hops,
		},
		AttemptTime: time.Date(2024, 5, 1, 18, 1, 0, 0, time.UTC),
		Hash:        (*lntypes.Hash)(&hash),
	}

	attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
	copy(attemptKey, htlcAttemptInfoKey)
	byteOrder.PutUint64(attemptKey[len(htlcAttemptInfoKey):], 1)

	var ab bytes.Buffer
	err = serializeHTLCAttemptInfo(&ab, attemptInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(attemptKey, ab.Bytes())
	if err != nil {
		return err
	}

	settleInfo := &HTLCSettleInfo{
		Preimage:   lntypes.Preimage(hash),
		SettleTime: time.Date(2024, 5, 1, 18, 2, 0, 0, time.UTC),
	}

	settleKey := make([]byte, len(htlcSettleInfoKey)+8)
	copy(settleKey, htlcSettleInfoKey)
	byteOrder.PutUint64(settleKey[len(htlcSettleInfoKey):], 1)

	var sb bytes.Buffer
	err = serializeHTLCSettleInfo(&sb, settleInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(settleKey, sb.Bytes())
	if err != nil {
		return err
	}

	var idx bytes.Buffer
	err = WriteElements(&idx, paymentIndexTypeHash, hash[:])
	if err != nil {
		return err
	}
	return indexBucket.Put(seqBytes, idx.Bytes())
}

// createPaymentWithAllFeatures creates a payment with all optional features
// enabled.
func createPaymentWithAllFeatures(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, hash [32]byte) error {
	t.Helper()

	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(hash[:])
	if err != nil {
		return err
	}

	var paymentID lntypes.Hash
	copy(paymentID[:], hash[:])

	// Payment with all features: payment-level custom records.
	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(150000),
		CreationTime:      time.Date(2024, 6, 1, 20, 0, 0, 0, time.UTC),
		PaymentRequest:    []byte("lnbc1500n1test_all_features"),
		FirstHopCustomRecords: lnwire.CustomRecords{
			65543: []byte("all_features_payment_custom_1"),
			65544: []byte("all_features_payment_custom_2"),
		},
	}

	var b bytes.Buffer
	err = serializePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = paymentBucket.Put(paymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, 6)
	err = paymentBucket.Put(paymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
		paymentHtlcsBucket,
	)
	if err != nil {
		return err
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	// Create route with all features: MPP, custom records, blinded route,
	// metadata.
	hops := make([]*route.Hop, 4)
	for i := 0; i < 4; i++ {
		hopKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		amt := lnwire.MilliSatoshi(150000 - uint64(i)*250)
		hop := &route.Hop{
			PubKeyBytes:      route.NewVertex(hopKey.PubKey()),
			ChannelID:        uint64(600000 + i),
			OutgoingTimeLock: uint32(1000000 - i*40),
			AmtToForward:     amt,
			// Hop-level custom records.
			CustomRecords: record.CustomSet{
				65545 + uint64(i): []byte(fmt.Sprintf(
					"all_feat_hop_%d", i,
				)),
			},
			// Hop metadata.
			Metadata: []byte(
				fmt.Sprintf("all_feat_metadata_%d", i),
			),
		}

		// Add MPP and blinded route data to final hop.
		if i == 3 {
			var paymentAddr [32]byte
			copy(
				paymentAddr[:],
				[]byte("all_features_mpp_addr_123456"),
			)
			hop.MPP = record.NewMPP(
				lnwire.MilliSatoshi(149250),
				paymentAddr,
			)

			blindingKey, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			hop.BlindingPoint = blindingKey.PubKey()
			hop.EncryptedData = []byte(
				"all_features_encrypted_blinded_data_123456",
			)
			hop.TotalAmtMsat = lnwire.MilliSatoshi(149250)
		}

		hops[i] = hop
	}

	attemptInfo := &HTLCAttemptInfo{
		AttemptID:  1,
		sessionKey: sessionKeyBytes,
		Route: route.Route{
			TotalTimeLock: 1000000,
			TotalAmount:   lnwire.MilliSatoshi(149250),
			SourcePubKey:  sourcePubKey,
			Hops:          hops,
			// Attempt-level first hop custom records.
			FirstHopWireCustomRecords: lnwire.CustomRecords{
				65549: []byte("all_feat_attempt_custom_1"),
				65550: []byte("all_feat_attempt_custom_2"),
			},
		},
		AttemptTime: time.Date(2024, 6, 1, 20, 1, 0, 0, time.UTC),
		Hash:        (*lntypes.Hash)(&hash),
	}

	attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
	copy(attemptKey, htlcAttemptInfoKey)
	byteOrder.PutUint64(attemptKey[len(htlcAttemptInfoKey):], 1)

	var ab bytes.Buffer
	err = serializeHTLCAttemptInfo(&ab, attemptInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(attemptKey, ab.Bytes())
	if err != nil {
		return err
	}

	settleInfo := &HTLCSettleInfo{
		Preimage:   lntypes.Preimage(hash),
		SettleTime: time.Date(2024, 6, 1, 20, 2, 0, 0, time.UTC),
	}

	settleKey := make([]byte, len(htlcSettleInfoKey)+8)
	copy(settleKey, htlcSettleInfoKey)
	byteOrder.PutUint64(settleKey[len(htlcSettleInfoKey):], 1)

	var sb bytes.Buffer
	err = serializeHTLCSettleInfo(&sb, settleInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(settleKey, sb.Bytes())
	if err != nil {
		return err
	}

	var idx bytes.Buffer
	err = WriteElements(&idx, paymentIndexTypeHash, hash[:])
	if err != nil {
		return err
	}

	return indexBucket.Put(seqBytes, idx.Bytes())
}

type paymentFeatureSet struct {
	name          string
	mpp           bool
	amp           bool
	customRecords bool
	blindedRoute  bool
	hopMetadata   bool
}

// createPaymentWithFeatureSet creates a payment with a selected set of
// optional features for combination testing.
func createPaymentWithFeatureSet(t *testing.T, paymentsBucket,
	indexBucket kvdb.RwBucket, hash [32]byte, seqNum uint64,
	features paymentFeatureSet, globalAttemptID *uint64) error {
	t.Helper()

	if features.mpp && features.amp {
		return fmt.Errorf("invalid feature set: mpp and amp")
	}

	paymentBucket, err := paymentsBucket.CreateBucketIfNotExists(hash[:])
	if err != nil {
		return err
	}

	var paymentID lntypes.Hash
	copy(paymentID[:], hash[:])

	creationTime := time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC).
		Add(time.Duration(seqNum) * time.Minute)
	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentID,
		Value:             lnwire.MilliSatoshi(100000),
		CreationTime:      creationTime,
		PaymentRequest: []byte(
			fmt.Sprintf("lnbc_test_%s", features.name),
		),
	}
	if features.customRecords {
		creationInfo.FirstHopCustomRecords = lnwire.CustomRecords{
			65560: []byte("combo_payment_custom_1"),
			65561: []byte("combo_payment_custom_2"),
		}
	}

	var b bytes.Buffer
	err = serializePaymentCreationInfo(&b, creationInfo)
	if err != nil {
		return err
	}
	err = paymentBucket.Put(paymentCreationInfoKey, b.Bytes())
	if err != nil {
		return err
	}

	seqBytes := make([]byte, 8)
	byteOrder.PutUint64(seqBytes, seqNum)
	err = paymentBucket.Put(paymentSequenceKey, seqBytes)
	if err != nil {
		return err
	}

	htlcBucket, err := paymentBucket.CreateBucketIfNotExists(
		paymentHtlcsBucket,
	)
	if err != nil {
		return err
	}

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var sourcePubKey route.Vertex
	copy(sourcePubKey[:], sessionKey.PubKey().SerializeCompressed())

	var sessionKeyBytes [32]byte
	copy(sessionKeyBytes[:], sessionKey.Serialize())

	baseAmt := lnwire.MilliSatoshi(100000)
	hops := make([]*route.Hop, 3)
	for i := 0; i < 3; i++ {
		hopKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		amt := baseAmt - lnwire.MilliSatoshi(uint64(i)*100)
		hop := &route.Hop{
			PubKeyBytes:      route.NewVertex(hopKey.PubKey()),
			ChannelID:        uint64(700000 + i),
			OutgoingTimeLock: uint32(700000 - i*40),
			AmtToForward:     amt,
		}
		if features.customRecords {
			hop.CustomRecords = record.CustomSet{
				65562 + uint64(i): []byte(fmt.Sprintf(
					"combo_hop_%d", i,
				)),
			}
		}
		if features.hopMetadata {
			hop.Metadata = []byte(
				fmt.Sprintf("combo_metadata_%d", i),
			)
		}

		if i == 2 {
			if features.mpp {
				var paymentAddr [32]byte
				copy(
					paymentAddr[:],
					[]byte("combo_mpp_payment_addr_1234"),
				)
				hop.MPP = record.NewMPP(
					baseAmt-200, paymentAddr,
				)
			}
			if features.amp {
				var rootShare [32]byte
				copy(
					rootShare[:],
					[]byte("combo_amp_root_share_123456"),
				)
				var setID [32]byte
				copy(
					setID[:],
					[]byte("combo_amp_set_id_12345678"),
				)
				hop.AMP = record.NewAMP(rootShare, setID, 0)
			}
			if features.blindedRoute {
				blindingKey, err := btcec.NewPrivateKey()
				require.NoError(t, err)
				hop.BlindingPoint = blindingKey.PubKey()
				hop.EncryptedData = []byte(
					"combo_encrypted_blinded_data",
				)
				hop.TotalAmtMsat = baseAmt - 200
			}
		}

		hops[i] = hop
	}

	routeInfo := route.Route{
		TotalTimeLock: 700000,
		TotalAmount:   baseAmt - 200,
		SourcePubKey:  sourcePubKey,
		Hops:          hops,
	}
	if features.customRecords {
		routeInfo.FirstHopWireCustomRecords = lnwire.CustomRecords{
			65565: []byte("combo_attempt_custom_1"),
			65566: []byte("combo_attempt_custom_2"),
		}
	}

	*globalAttemptID++
	attemptID := *globalAttemptID
	attemptInfo := &HTLCAttemptInfo{
		AttemptID:   attemptID,
		sessionKey:  sessionKeyBytes,
		Route:       routeInfo,
		AttemptTime: creationTime.Add(time.Minute),
		Hash:        (*lntypes.Hash)(&hash),
	}

	attemptKey := make([]byte, len(htlcAttemptInfoKey)+8)
	copy(attemptKey, htlcAttemptInfoKey)
	byteOrder.PutUint64(attemptKey[len(htlcAttemptInfoKey):], attemptID)

	var ab bytes.Buffer
	err = serializeHTLCAttemptInfo(&ab, attemptInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(attemptKey, ab.Bytes())
	if err != nil {
		return err
	}

	settleInfo := &HTLCSettleInfo{
		Preimage:   lntypes.Preimage(hash),
		SettleTime: creationTime.Add(2 * time.Minute),
	}

	settleKey := make([]byte, len(htlcSettleInfoKey)+8)
	copy(settleKey, htlcSettleInfoKey)
	byteOrder.PutUint64(settleKey[len(htlcSettleInfoKey):], attemptID)

	var sb bytes.Buffer
	err = serializeHTLCSettleInfo(&sb, settleInfo)
	if err != nil {
		return err
	}
	err = htlcBucket.Put(settleKey, sb.Bytes())
	if err != nil {
		return err
	}

	var idx bytes.Buffer
	err = WriteElements(&idx, paymentIndexTypeHash, hash[:])
	if err != nil {
		return err
	}

	return indexBucket.Put(seqBytes, idx.Bytes())
}
