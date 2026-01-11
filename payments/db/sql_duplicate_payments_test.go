//go:build test_db_postgres || test_db_sqlite

package paymentsdb

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestFetchDuplicatePayments verifies that duplicate payment records are
// returned with the expected failure or settlement data for a single payment.
func TestFetchDuplicatePayments(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	paymentDB, _ := NewTestDB(t)
	sqlStore, ok := paymentDB.(*SQLStore)
	require.True(t, ok)

	paymentHash := testHash
	createdAt := time.Unix(100, 0).UTC()
	amount := lnwire.MilliSatoshi(1000)

	dupFailedHash := makeTestHash(0x02)
	dupSettledHash := makeTestHash(0x03)

	settlePreimage := makeTestPreimage(0x10)
	settleTime := time.Unix(200, 0).UTC()

	duplicates := []sqlc.InsertPaymentDuplicateMigParams{
		{
			PaymentIdentifier: dupFailedHash[:],
			AmountMsat:        int64(amount),
			CreatedAt:         createdAt.Add(time.Second),
			FailReason: sql.NullInt32{
				Int32: int32(FailureReasonError),
				Valid: true,
			},
		},
		{
			PaymentIdentifier: dupSettledHash[:],
			AmountMsat:        int64(amount + 100),
			CreatedAt:         createdAt.Add(2 * time.Second),
			SettlePreimage:    settlePreimage[:],
			SettleTime: sql.NullTime{
				Time:  settleTime,
				Valid: true,
			},
		},
	}

	insertTestPaymentWithDuplicates(
		t, ctx, sqlStore, paymentHash, createdAt, amount, duplicates,
	)

	results, err := sqlStore.FetchDuplicatePayments(ctx, paymentHash)
	require.NoError(t, err)
	require.Len(t, results, 2)

	byHash := make(map[lntypes.Hash]*DuplicatePayment, len(results))
	for _, dup := range results {
		byHash[dup.PaymentIdentifier] = dup
	}

	failed := byHash[dupFailedHash]
	require.NotNil(t, failed)
	require.NotNil(t, failed.FailureReason)
	require.Equal(t, FailureReasonError, *failed.FailureReason)
	require.Nil(t, failed.Settle)

	settled := byHash[dupSettledHash]
	require.NotNil(t, settled)
	require.Nil(t, settled.FailureReason)
	require.NotNil(t, settled.Settle)
	require.Equal(t, settlePreimage, settled.Settle.Preimage)
	require.True(t, settled.Settle.SettleTime.Equal(settleTime))
}

// TestFetchAllDuplicatePayments verifies cursor-based pagination over all
// duplicate payment records across multiple payments.
func TestFetchAllDuplicatePayments(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	queryCfg := sqldb.DefaultSQLiteConfig()
	queryCfg.MaxPageSize = 1

	sqlStore, err := NewSQLStore(
		&SQLStoreConfig{
			QueryCfg: queryCfg,
		},
		newBatchQuerier(t),
	)
	require.NoError(t, err)

	createdAt := time.Unix(300, 0).UTC()
	amount := lnwire.MilliSatoshi(2000)

	paymentHashA := makeTestHash(0x11)
	dupA1 := makeTestHash(0x12)
	dupA2 := makeTestHash(0x13)

	paymentHashB := makeTestHash(0x21)
	dupB1 := makeTestHash(0x22)

	insertTestPaymentWithDuplicates(
		t, ctx, sqlStore, paymentHashA, createdAt, amount,
		[]sqlc.InsertPaymentDuplicateMigParams{
			{
				PaymentIdentifier: dupA1[:],
				AmountMsat:        int64(amount),
				CreatedAt:         createdAt.Add(time.Second),
				FailReason: sql.NullInt32{
					Int32: int32(FailureReasonError),
					Valid: true,
				},
			},
			{
				PaymentIdentifier: dupA2[:],
				AmountMsat:        int64(amount + 10),
				CreatedAt: createdAt.Add(
					2 * time.Second,
				),
				FailReason: sql.NullInt32{
					Int32: int32(FailureReasonError),
					Valid: true,
				},
			},
		},
	)

	insertTestPaymentWithDuplicates(
		t, ctx, sqlStore, paymentHashB, createdAt, amount,
		[]sqlc.InsertPaymentDuplicateMigParams{
			{
				PaymentIdentifier: dupB1[:],
				AmountMsat:        int64(amount + 20),
				CreatedAt: createdAt.Add(
					3 * time.Second,
				),
				FailReason: sql.NullInt32{
					Int32: int32(FailureReasonError),
					Valid: true,
				},
			},
		},
	)

	results, err := sqlStore.FetchAllDuplicatePayments(ctx)
	require.NoError(t, err)
	require.Len(t, results, 3)

	require.Equal(t, dupA1, results[0].PaymentIdentifier)
	require.Equal(t, dupA2, results[1].PaymentIdentifier)
	require.Equal(t, dupB1, results[2].PaymentIdentifier)
}

// insertTestPaymentWithDuplicates inserts a test payment with duplicates
// into the database.

func insertTestPaymentWithDuplicates(t *testing.T, ctx context.Context,
	sqlStore *SQLStore, paymentHash lntypes.Hash, createdAt time.Time,
	amount lnwire.MilliSatoshi,
	duplicates []sqlc.InsertPaymentDuplicateMigParams) {

	t.Helper()

	err := sqlStore.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
			paymentID, err := db.InsertPaymentMig(
				ctx, sqlc.InsertPaymentMigParams{
					AmountMsat:        int64(amount),
					CreatedAt:         createdAt,
					PaymentIdentifier: paymentHash[:],
				},
			)
			if err != nil {
				return err
			}

			for i := range duplicates {
				dup := duplicates[i]
				dup.PaymentID = paymentID
				_, err := db.InsertPaymentDuplicateMig(ctx, dup)
				if err != nil {
					return err
				}
			}

			return nil
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)
}

// makeTestHash creates a test hash with the given byte.
func makeTestHash(b byte) lntypes.Hash {
	var hash lntypes.Hash
	for i := 0; i < len(hash); i++ {
		hash[i] = b
	}
	return hash
}

// makeTestPreimage creates a test preimage with the given byte.
func makeTestPreimage(b byte) lntypes.Preimage {
	var preimage lntypes.Preimage
	for i := 0; i < len(preimage); i++ {
		preimage[i] = b
	}
	return preimage
}
