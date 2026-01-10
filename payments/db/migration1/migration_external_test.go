//go:build test_db_postgres || test_db_sqlite

package migration1

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/payments/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestMigrationWithExternalDB tests the migration of the payment store from a
// bolt backed channel.db or a kvdb channel.sqlite to a SQL database. Note that
// this test does not attempt to be a complete migration test for all payment
// store types but rather is added as a tool for developers and users to debug
// payment migration issues with an actual channel.db/channel.sqlite file.
//
// NOTE: To use this test, place either of those files in the
// payments/db/migration1/testdata directory, uncomment the "Skipf" line, and
// set the "fileName" variable to the name of the channel database file you
// want to use for the migration test.
func TestMigrationWithExternalDB(t *testing.T) {
	ctx := context.Background()

	// NOTE: comment this line out to run the test.
	t.Skipf("skipping test meant for local debugging only")

	// NOTE: set this to the name of the channel database file you want
	// to use for the migration test. This may be either a bbolt ".db" file
	// or a SQLite ".sqlite" file. If you want to migrate from a
	// bbolt channel.db file, set this to "channel.db".
	const fileName = "channel.db"

	// NOTE: if set, this test will prefer migrating from a Postgres-backed
	// kvdb source instead of a local file. Leave empty to use fileName.
	const postgresKVDSN = ""
	const postgresKVPfx = "channeldb"
	const logSequenceOrder = false

	// Determine if we are using a SQLite file or a Bolt DB file.
	isSqlite := strings.HasSuffix(fileName, ".sqlite")

	// Set up logging for the test.
	logger := btclog.NewSLogger(btclog.NewDefaultHandler(os.Stdout))
	UseLogger(logger)

	// migrate runs the migration from the kvdb store to the SQL store
	// and then performs a batched deep comparison of every migrated
	// payment to ensure all data (including HTLC details) was preserved
	// correctly.
	migrate := func(t *testing.T, kvBackend kvdb.Backend) {
		sqlStore := setupTestSQLDB(t)

		// Run migration in a transaction.
		err := sqlStore.db.ExecTx(
			ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
				return MigratePaymentsKVToSQL(
					ctx, kvBackend, tx, &SQLStoreConfig{
						QueryCfg: sqlStore.cfg.QueryCfg,
					},
				)
			}, sqldb.NoOpReset,
		)
		require.NoError(t, err)

		t.Log("========================================")
		t.Log("   Deep Validation")
		t.Log("========================================")

		// Deep compare all migrated payments in batches.
		deepValidateAllPayments(
			t, ctx, kvBackend, sqlStore,
		)

		_ = logSequenceOrder
	}

	connectPostgres := func(t *testing.T, dsn, prefix string) kvdb.Backend {
		dsn = strings.TrimSpace(dsn)
		if dsn == "" {
			t.Fatalf("missing postgres kvdb dsn")
		}

		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			prefix = "channeldb"
		}

		const (
			timeout  = 10 * time.Second
			maxConns = 5
		)
		sqlbase.Init(maxConns)

		dbCfg := &postgres.Config{
			Dsn:            dsn,
			Timeout:        timeout,
			MaxConnections: maxConns,
		}

		kvStore, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx, dbCfg, prefix,
		)
		require.NoError(t, err)

		t.Cleanup(func() { _ = kvStore.Close() })

		return kvStore
	}

	connectPostgresKV := func(t *testing.T) kvdb.Backend {
		return connectPostgres(t, postgresKVDSN, postgresKVPfx)
	}

	connectBBolt := func(t *testing.T, dbPath string) kvdb.Backend {
		cfg := &kvdb.BoltBackendConfig{
			DBPath:            dbPath,
			DBFileName:        fileName,
			NoFreelistSync:    true,
			AutoCompact:       false,
			AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
			DBTimeout:         kvdb.DefaultDBTimeout,
			ReadOnly:          true,
		}

		kvStore, err := kvdb.GetBoltBackend(cfg)
		require.NoError(t, err)

		t.Cleanup(func() { _ = kvStore.Close() })

		return kvStore
	}

	connectSQLite := func(t *testing.T, dbPath string) kvdb.Backend {
		const (
			timeout  = 10 * time.Second
			maxConns = 5
		)
		sqlbase.Init(maxConns)

		cfg := &sqlite.Config{
			Timeout:        timeout,
			BusyTimeout:    timeout,
			MaxConnections: maxConns,
		}

		kvStore, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, cfg,
			dbPath, fileName,
			// NOTE: we use the raw string here else we get an
			// import cycle if we try to import lncfg.NSChannelDB.
			"channeldb",
		)
		require.NoError(t, err)

		t.Cleanup(func() { _ = kvStore.Close() })

		return kvStore
	}

	tests := []struct {
		name   string
		dbPath string
	}{
		{
			name:   "testdata",
			dbPath: "testdata",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if postgresKVDSN != "" {
				migrate(t, connectPostgresKV(t))
				return
			}

			chanDBPath := path.Join(test.dbPath, fileName)
			t.Logf("Connecting to channel DB at: %s", chanDBPath)

			connectDB := connectBBolt
			if isSqlite {
				connectDB = connectSQLite
			}

			migrate(t, connectDB(t, test.dbPath))
		})
	}
}

// kvPaymentRef holds a KV payment and its hash for batched deep validation.
type kvPaymentRef struct {
	hash    lntypes.Hash
	payment *MPPayment
}

// deepValidateAllPayments iterates all KV payments in batches and performs a
// deep comparison against their SQL counterparts. For each batch, it fetches
// the full SQL payment data (including HTLCs, routes, custom records) via
// batch queries and compares field-by-field with the KV data.
func deepValidateAllPayments(t *testing.T, ctx context.Context,
	kvBackend kvdb.Backend, sqlStore *SQLStore) {

	t.Helper()

	batchSize := int(sqlStore.cfg.QueryCfg.MaxBatchSize)

	// Get total payment count from SQL so we can show progress.
	var totalPayments int64
	err := sqlStore.db.ExecTx(
		ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
			var err error
			totalPayments, err = db.CountPayments(ctx)
			return err
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)

	var totalValidated int

	// Iterate all KV payments, collecting them into batches. For each
	// full batch, perform a batched deep comparison against SQL.
	var batch []kvPaymentRef
	err = kvdb.View(kvBackend, func(kvTx kvdb.RTx) error {
		paymentsBucket := kvTx.ReadBucket(paymentsRootBucket)
		if paymentsBucket == nil {
			return nil
		}

		return paymentsBucket.ForEach(func(k, v []byte) error {
			bucket := paymentsBucket.NestedReadBucket(k)
			if bucket == nil {
				return nil
			}

			payment, err := fetchPayment(bucket)
			if err != nil {
				return err
			}

			var hash lntypes.Hash
			copy(hash[:], k)

			batch = append(batch, kvPaymentRef{
				hash:    hash,
				payment: payment,
			})

			if len(batch) >= batchSize {
				deepCompareBatch(
					t, ctx, sqlStore, batch,
				)
				totalValidated += len(batch)
				t.Logf("Deep validated %d/%d payments",
					totalValidated, totalPayments)

				batch = batch[:0]
			}

			return nil
		})
	}, func() {
		batch = nil
		totalValidated = 0
	})
	require.NoError(t, err)

	// Validate any remaining payments in the last batch.
	if len(batch) > 0 {
		deepCompareBatch(t, ctx, sqlStore, batch)
		totalValidated += len(batch)
	}

	t.Logf("Deep validated %d/%d payments (complete)",
		totalValidated, totalPayments)
}

// deepCompareBatch performs a batched deep comparison of KV payments against
// their SQL counterparts. It fetches SQL payment data in a single read
// transaction using batch queries.
func deepCompareBatch(t *testing.T, ctx context.Context,
	sqlStore *SQLStore, batch []kvPaymentRef) {

	t.Helper()

	err := sqlStore.db.ExecTx(
		ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
			// Look up each payment by hash to get the SQL row
			// with payment ID.
			ids := make([]int64, 0, len(batch))
			rowsByHash := make(
				map[lntypes.Hash]sqlc.FetchPaymentsByIDsRow,
				len(batch),
			)
			for _, ref := range batch {
				row, err := fetchPaymentByHash(
					ctx, db, ref.hash,
				)
				if err != nil {
					return fmt.Errorf("fetch SQL payment "+
						"%x: %w", ref.hash[:8], err)
				}
				ids = append(ids, row.Payment.ID)
			}

			// Batch-fetch full payment details.
			rows, err := db.FetchPaymentsByIDs(ctx, ids)
			if err != nil {
				return fmt.Errorf("batch fetch payments: %w",
					err)
			}
			for _, row := range rows {
				var hash lntypes.Hash
				copy(hash[:], row.PaymentIdentifier)
				rowsByHash[hash] = row
			}

			batchData, err := batchLoadPaymentDetailsData(
				ctx, sqlStore.cfg.QueryCfg, db, ids,
			)
			if err != nil {
				return fmt.Errorf("batch load details: %w",
					err)
			}

			// Deep compare each payment.
			for _, ref := range batch {
				row, ok := rowsByHash[ref.hash]
				require.True(t, ok,
					"SQL payment %x not in batch "+
						"results", ref.hash[:8])

				sqlPayment, err := buildPaymentFromBatchData(
					row, batchData,
				)
				require.NoError(t, err,
					"build SQL payment %x",
					ref.hash[:8])

				normalizePaymentData(ref.payment)
				normalizePaymentData(sqlPayment)

				require.Equal(
					t, ref.payment, sqlPayment,
					"payment mismatch %x",
					ref.hash[:8],
				)
			}

			return nil
		}, sqldb.NoOpReset,
	)
	require.NoError(t, err)
}
