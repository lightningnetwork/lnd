package paymentsdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// benchmarkParallelism controls GOMAXPROCS multiplier for concurrent tests.
const benchmarkParallelism = 4

// preloadedPayments is the number of payments inserted before timing
// begins to simulate a database with existing history.
const preloadedPayments = 50000

// testdataDir is the directory containing pre-populated database files.
const testdataDir = "testdata"

// testdataSQLitePath is the path to the pre-populated SQLite database.
const testdataSQLitePath = testdataDir + "/payments.sqlite"

// testdataBBoltDir is the directory containing the pre-populated BBolt
// database.
const testdataBBoltDir = testdataDir + "/kvdb"

// testdataBBoltFile is the filename of the pre-populated BBolt database.
const testdataBBoltFile = "payments.db"

// dbConnection represents a database connection configuration for benchmarks.
type dbConnection struct {
	name string
	open func(testing.TB) DB
}

// connectBBoltDB creates a new BBolt-backed payment store for benchmarking.
func connectBBoltDB(t testing.TB) DB {
	t.Helper()

	cfg := &kvdb.BoltBackendConfig{
		DBPath:            t.TempDir(),
		DBFileName:        "payments.db",
		NoFreelistSync:    true,
		AutoCompact:       false,
		AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
		DBTimeout:         kvdb.DefaultDBTimeout,
	}

	backend, err := kvdb.GetBoltBackend(cfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, backend.Close())
	})

	store, err := NewKVStore(backend)
	require.NoError(t, err)

	return store
}

// connectSQLite creates a new native SQLite-backed payment store for
// benchmarking. Uses the same pattern as test_sqlite.go.
func connectSQLite(t testing.TB) DB {
	t.Helper()

	db := sqldb.NewTestSqliteDB(t).BaseDB

	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)

	store, err := NewSQLStore(
		&SQLStoreConfig{
			QueryCfg: sqldb.DefaultSQLiteConfig(),
		}, executor,
	)
	require.NoError(t, err)

	return store
}

// connectPostgres creates a new Postgres-backed payment store for
// benchmarking. Uses the same pattern as test_postgres.go.
func connectPostgres(t testing.TB) DB {
	t.Helper()

	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	db := sqldb.NewTestPostgresDB(t, pgFixture).BaseDB

	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)

	store, err := NewSQLStore(
		&SQLStoreConfig{
			QueryCfg: sqldb.DefaultPostgresConfig(),
		}, executor,
	)
	require.NoError(t, err)

	return store
}

// benchmarkBackends returns the list of database backends to benchmark.
func benchmarkBackends() []dbConnection {
	return []dbConnection{
		{name: "bbolt", open: connectBBoltDB},
		{name: "sqlite", open: connectSQLite},
		{name: "postgres", open: connectPostgres},
	}
}

// connectExistingSQLite copies the pre-populated SQLite database from testdata
// to a temp directory and opens it. This avoids modifying the original file
// since benchmarks write to the database.
func connectExistingSQLite(t testing.TB) DB {
	t.Helper()

	srcPath := testdataSQLitePath
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		t.Skipf("Pre-populated SQLite DB not found at %s; "+
			"run TestPopulateDB first", srcPath)
	}

	// Copy to temp directory since the benchmark writes.
	tmpDir := t.TempDir()
	dstPath := filepath.Join(tmpDir, "payments.sqlite")
	copyFile(t, srcPath, dstPath)

	sqlDB, err := sqldb.NewSqliteStore(&sqldb.SqliteConfig{}, dstPath)
	require.NoError(t, err)

	// Apply migrations in case the schema has been updated since the
	// testdata was generated. This is idempotent and skips already-applied
	// migrations.
	require.NoError(t, sqlDB.ApplyAllMigrations(
		context.Background(), sqldb.GetMigrations(),
	))

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	executor := sqldb.NewTransactionExecutor(
		sqlDB.BaseDB, func(tx *sql.Tx) SQLQueries {
			return sqlDB.BaseDB.WithTx(tx)
		},
	)

	store, err := NewSQLStore(
		&SQLStoreConfig{
			QueryCfg: sqldb.DefaultSQLiteConfig(),
		}, executor,
	)
	require.NoError(t, err)

	return store
}

// connectExistingBBolt copies the pre-populated BBolt database from testdata
// to a temp directory and opens it. This avoids modifying the original file
// since benchmarks write to the database.
func connectExistingBBolt(t testing.TB) DB {
	t.Helper()

	srcPath := filepath.Join(testdataBBoltDir, testdataBBoltFile)
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		t.Skipf("Pre-populated BBolt DB not found at %s; "+
			"run TestPopulateDB first", srcPath)
	}

	// Copy to temp directory since the benchmark writes.
	tmpDir := t.TempDir()
	copyFile(t, srcPath, filepath.Join(tmpDir, testdataBBoltFile))

	backend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            tmpDir,
		DBFileName:        testdataBBoltFile,
		NoFreelistSync:    true,
		AutoCompact:       false,
		AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
		DBTimeout:         kvdb.DefaultDBTimeout,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, backend.Close())
	})

	store, err := NewKVStore(backend)
	require.NoError(t, err)

	return store
}

// copyFile copies a file from src to dst.
func copyFile(t testing.TB, src, dst string) {
	t.Helper()

	srcFile, err := os.Open(src)
	require.NoError(t, err)
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	require.NoError(t, err)
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	require.NoError(t, err)

	require.NoError(t, dstFile.Sync())
}

// benchmarkPaymentData holds pre-generated data for a single payment in
// benchmarks.
type benchmarkPaymentData struct {
	creationInfo *PaymentCreationInfo
	preimage     lntypes.Preimage
	hash         lntypes.Hash
	attempts     []*HTLCAttemptInfo
}

// generatePaymentData generates data for a single payment with the specified
// number of attempts.
func generatePaymentData(tb testing.TB, attemptIDStart uint64,
	numAttempts int) benchmarkPaymentData {

	tb.Helper()

	var preimage lntypes.Preimage
	_, err := io.ReadFull(rand.Reader, preimage[:])
	require.NoError(tb, err)

	hash := lntypes.Hash(sha256.Sum256(preimage[:]))

	// Scale the payment value by the number of attempts so all attempts
	// can be registered without exceeding the payment amount.
	paymentValue := testRoute.ReceiverAmt()
	if numAttempts > 1 {
		paymentValue *= lnwire.MilliSatoshi(numAttempts)
	}

	data := benchmarkPaymentData{
		creationInfo: &PaymentCreationInfo{
			PaymentIdentifier: hash,
			Value:             paymentValue,
			CreationTime:      time.Unix(time.Now().Unix(), 0),
			PaymentRequest:    []byte("lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaq"),
		},
		preimage: preimage,
		hash:     hash,
		attempts: make([]*HTLCAttemptInfo, numAttempts),
	}

	for j := range numAttempts {
		sessionKey, err := btcec.NewPrivateKey()
		require.NoError(tb, err)

		attempt, err := NewHtlcAttempt(
			attemptIDStart+uint64(j), sessionKey,
			*testRoute.Copy(), time.Now(), &hash,
		)
		require.NoError(tb, err)

		data.attempts[j] = &attempt.HTLCAttemptInfo
	}

	return data
}

// preloadPayments inserts n payments before the benchmark timer starts to
// simulate a database with existing history.
func preloadPayments(b *testing.B, store DB, n int) {
	b.Helper()

	if n == 0 {
		return
	}

	b.StopTimer()
	b.Logf("Preloading %d payments", n)

	ctx := b.Context()
	for i := range n {
		pd := generatePaymentData(b, uint64(i), 0)
		err := store.InitPayment(ctx, pd.hash, pd.creationInfo)
		require.NoError(b, err)
	}

	b.StartTimer()
}

// populatePayments inserts payments with a realistic distribution into the
// given store. It writes progress every 1000 payments.
//
//   - ~70% settled (init → register 1 attempt with 2 hops → settle)
//   - ~20% failed  (init → register 1-2 attempts → fail)
//   - ~10% multi-attempt settled (init → register → fail → register → settle)
func populatePayments(t *testing.T, store DB, n int) {
	t.Helper()

	ctx := context.Background()
	var attemptID uint64

	for i := range n {
		if i > 0 && i%1000 == 0 {
			t.Logf("Populated %d / %d payments", i, n)
		}

		pd := generatePaymentData(t, attemptID, 0)

		err := store.InitPayment(ctx, pd.hash, pd.creationInfo)
		require.NoError(t, err)

		switch {
		// ~70%: single-attempt settled.
		case i%10 < 7:
			attempt := generateAttempt(t, attemptID, pd.hash)
			attemptID++

			_, err = store.RegisterAttempt(
				ctx, pd.hash, attempt,
			)
			require.NoError(t, err)

			_, err = store.SettleAttempt(
				ctx, pd.hash, attempt.AttemptID,
				&HTLCSettleInfo{
					Preimage:   pd.preimage,
					SettleTime: time.Now(),
				},
			)
			require.NoError(t, err)

		// ~20%: failed payment (1-2 attempts, all failed).
		case i%10 < 9:
			numAttempts := 1
			if i%2 == 0 {
				numAttempts = 2
			}

			for range numAttempts {
				attempt := generateAttempt(
					t, attemptID, pd.hash,
				)
				attemptID++

				_, err = store.RegisterAttempt(
					ctx, pd.hash, attempt,
				)
				require.NoError(t, err)

				_, err = store.FailAttempt(
					ctx, pd.hash,
					attempt.AttemptID,
					&HTLCFailInfo{
						FailTime: time.Now(),
						Reason:   HTLCFailUnreadable,
					},
				)
				require.NoError(t, err)
			}

			_, err = store.Fail(
				ctx, pd.hash, FailureReasonNoRoute,
			)
			require.NoError(t, err)

		// ~10%: multi-attempt settled (first attempt fails, second
		// settles).
		default:
			first := generateAttempt(t, attemptID, pd.hash)
			attemptID++

			_, err = store.RegisterAttempt(
				ctx, pd.hash, first,
			)
			require.NoError(t, err)

			_, err = store.FailAttempt(
				ctx, pd.hash, first.AttemptID,
				&HTLCFailInfo{
					FailTime: time.Now(),
					Reason:   HTLCFailUnreadable,
				},
			)
			require.NoError(t, err)

			second := generateAttempt(t, attemptID, pd.hash)
			attemptID++

			_, err = store.RegisterAttempt(
				ctx, pd.hash, second,
			)
			require.NoError(t, err)

			_, err = store.SettleAttempt(
				ctx, pd.hash, second.AttemptID,
				&HTLCSettleInfo{
					Preimage:   pd.preimage,
					SettleTime: time.Now(),
				},
			)
			require.NoError(t, err)
		}
	}

	t.Logf("Populated %d / %d payments (done)", n, n)
}

// generateAttempt creates a single HTLC attempt for the given payment hash.
func generateAttempt(t testing.TB, attemptID uint64,
	hash lntypes.Hash) *HTLCAttemptInfo {

	t.Helper()

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	attempt, err := NewHtlcAttempt(
		attemptID, sessionKey, *testRoute.Copy(), time.Now(), &hash,
	)
	require.NoError(t, err)

	return &attempt.HTLCAttemptInfo
}

// TestPopulateDB generates pre-populated SQLite and BBolt databases in the
// testdata directory. This test is skipped by default and should be run
// manually when the testdata needs to be (re)generated:
//
//	go test -run TestPopulateDB -v -count=1 -tags test_db_sqlite ./payments/db/...
func TestPopulateDB(t *testing.T) {
	t.Skip("Run manually to generate pre-populated benchmark databases")

	require.NoError(t, os.MkdirAll(testdataDir, 0750))
	require.NoError(t, os.MkdirAll(testdataBBoltDir, 0750))

	t.Run("sqlite", func(t *testing.T) {
		dbPath := testdataSQLitePath

		// Remove any existing file so we start fresh.
		_ = os.Remove(dbPath)

		sqlDB, err := sqldb.NewSqliteStore(
			&sqldb.SqliteConfig{}, dbPath,
		)
		require.NoError(t, err)

		require.NoError(t, sqlDB.ApplyAllMigrations(
			context.Background(), sqldb.GetMigrations(),
		))

		executor := sqldb.NewTransactionExecutor(
			sqlDB.BaseDB, func(tx *sql.Tx) SQLQueries {
				return sqlDB.BaseDB.WithTx(tx)
			},
		)

		store, err := NewSQLStore(
			&SQLStoreConfig{
				QueryCfg: sqldb.DefaultSQLiteConfig(),
			}, executor,
		)
		require.NoError(t, err)

		populatePayments(t, store, preloadedPayments)

		require.NoError(t, sqlDB.DB.Close())
	})

	t.Run("bbolt", func(t *testing.T) {
		dbPath := filepath.Join(testdataBBoltDir, testdataBBoltFile)

		// Remove any existing file so we start fresh.
		_ = os.Remove(dbPath)

		backend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
			DBPath:            testdataBBoltDir,
			DBFileName:        testdataBBoltFile,
			NoFreelistSync:    true,
			AutoCompact:       false,
			AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
			DBTimeout:         kvdb.DefaultDBTimeout,
		})
		require.NoError(t, err)

		store, err := NewKVStore(backend)
		require.NoError(t, err)

		populatePayments(t, store, preloadedPayments)

		require.NoError(t, backend.Close())
	})
}

// benchAttemptIDOffset is the starting attempt ID for benchmark iterations,
// chosen to avoid collisions with attempt IDs from the pre-populated testdata.
const benchAttemptIDOffset = preloadedPayments * 10

// BenchmarkPayments benchmarks the full payment lifecycle against
// pre-populated databases from testdata. Each iteration performs:
// Init → Register → Fail → Fetch → Register → Settle → DeleteFailed.
//
// Generate testdata first:
//
//	go test -run TestPopulateDB -v -count=1 -tags test_db_sqlite ./payments/db/...
//
// Then run:
//
//	go test -bench BenchmarkPayments -benchtime=100x -count=1 -tags test_db_sqlite ./payments/db/...
func BenchmarkPayments(b *testing.B) {
	backends := []dbConnection{
		{name: "bbolt", open: connectExistingBBolt},
		{name: "sqlite", open: connectExistingSQLite},
	}

	for _, backend := range backends {
		b.Run(backend.name, func(b *testing.B) {
			store := backend.open(b)
			ctx := b.Context()

			// Pre-generate all payment data. Each payment needs
			// 2 attempts: one to fail, one to settle. Offset
			// attempt IDs to avoid collisions with pre-populated
			// data.
			paymentPool := make([]benchmarkPaymentData, b.N)
			for i := range b.N {
				paymentPool[i] = generatePaymentData(
					b, benchAttemptIDOffset+uint64(i*2),
					2,
				)
			}

			var idx atomic.Uint64
			b.ResetTimer()
			b.SetParallelism(benchmarkParallelism)

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := idx.Add(1) - 1
					pd := paymentPool[i]

					// 1. Init payment.
					err := store.InitPayment(
						ctx, pd.hash, pd.creationInfo,
					)
					if err != nil {
						b.Errorf("InitPayment: %v", err)
						return
					}

					// 2. Register first attempt.
					_, err = store.RegisterAttempt(
						ctx, pd.hash, pd.attempts[0],
					)
					if err != nil {
						b.Errorf("RegisterAttempt: %v",
							err)
						return
					}

					// 3. First attempt fails.
					_, err = store.FailAttempt(
						ctx, pd.hash,
						pd.attempts[0].AttemptID,
						&HTLCFailInfo{
							FailTime: time.Now(),
							Reason:   HTLCFailUnreadable,
						},
					)
					if err != nil {
						b.Errorf("FailAttempt: %v", err)
						return
					}

					// 4. Fetch payment state.
					_, err = store.FetchPayment(
						ctx, pd.hash,
					)
					if err != nil {
						b.Errorf("FetchPayment: %v", err)
						return
					}

					// 5. Register second attempt.
					_, err = store.RegisterAttempt(
						ctx, pd.hash, pd.attempts[1],
					)
					if err != nil {
						b.Errorf("RegisterAttempt: %v",
							err)
						return
					}

					// 6. Settle second attempt.
					_, err = store.SettleAttempt(
						ctx, pd.hash,
						pd.attempts[1].AttemptID,
						&HTLCSettleInfo{
							Preimage:   pd.preimage,
							SettleTime: time.Now(),
						},
					)
					if err != nil {
						b.Errorf("SettleAttempt: %v",
							err)
						return
					}

					// 7. Cleanup failed attempts.
					err = store.DeleteFailedAttempts(
						ctx, pd.hash,
					)
					if err != nil {
						b.Errorf("DeleteFailed: %v",
							err)
						return
					}
				}
			})
		})
	}
}

// queryBenchCase defines a single QueryPayments sub-benchmark.
type queryBenchCase struct {
	name  string
	query Query
}

// BenchmarkQueryPayments benchmarks QueryPayments (the ListPayments RPC path)
// against pre-populated databases from testdata.
//
// Generate testdata first:
//
//	go test -run TestPopulateDB -v -count=1 -tags test_db_sqlite ./payments/db/...
//
// Then run:
//
//	go test -bench BenchmarkQueryPayments -benchtime=100x -count=1 -tags test_db_sqlite ./payments/db/...
func BenchmarkQueryPayments(b *testing.B) {
	backends := []dbConnection{
		{name: "bbolt", open: connectExistingBBolt},
		{name: "sqlite", open: connectExistingSQLite},
	}

	cases := []queryBenchCase{
		{
			name: "default_page",
			query: Query{
				MaxPayments: 100,
			},
		},
		{
			name: "large_page",
			query: Query{
				MaxPayments: 1000,
			},
		},
		{
			name: "reversed",
			query: Query{
				MaxPayments: 100,
				Reversed:    true,
			},
		},
		{
			name: "include_incomplete",
			query: Query{
				MaxPayments:       100,
				IncludeIncomplete: true,
			},
		},
		{
			name: "omit_hops",
			query: Query{
				MaxPayments:       100,
				IncludeIncomplete: true,
				OmitHops:          true,
			},
		},
		{
			name: "fetch_all",
			query: Query{
				MaxPayments:       preloadedPayments,
				IncludeIncomplete: true,
			},
		},
	}

	for _, backend := range backends {
		b.Run(backend.name, func(b *testing.B) {
			store := backend.open(b)
			ctx := b.Context()

			for _, tc := range cases {
				b.Run(tc.name, func(b *testing.B) {
					query := tc.query

					b.ResetTimer()
					b.SetParallelism(
						benchmarkParallelism,
					)

					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							_, err := store.QueryPayments( //nolint:lll
								ctx, query,
							)
							if err != nil {
								b.Errorf("QueryPayments: %v", err) //nolint:lll
								return
							}
						}
					})
				})
			}
		})
	}
}

// BenchmarkConcurrentPaymentFlow benchmarks the realistic sendpayment flow
// under concurrent load. Each iteration performs the full lifecycle:
// Init → Register → Fail → Fetch → Register → Settle → DeleteFailed.
// The database is preloaded with historical payments before timing begins.
func BenchmarkConcurrentPaymentFlow(b *testing.B) {
	for _, backend := range benchmarkBackends() {
		b.Run(backend.name, func(b *testing.B) {
			store := backend.open(b)
			ctx := b.Context()

			preloadPayments(b, store, preloadedPayments)

			// Pre-generate all payment data. Each payment needs
			// 2 attempts: one to fail, one to settle.
			paymentPool := make([]benchmarkPaymentData, b.N)
			for i := range b.N {
				paymentPool[i] = generatePaymentData(
					b, uint64(i*2), 2,
				)
			}

			var idx atomic.Uint64
			b.ResetTimer()
			b.SetParallelism(benchmarkParallelism)

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := idx.Add(1) - 1
					pd := paymentPool[i]

					// 1. Init payment.
					err := store.InitPayment(
						ctx, pd.hash, pd.creationInfo,
					)
					if err != nil {
						b.Errorf("InitPayment: %v", err)
						return
					}

					// 2. Register first attempt.
					_, err = store.RegisterAttempt(
						ctx, pd.hash, pd.attempts[0],
					)
					if err != nil {
						b.Errorf("RegisterAttempt: %v",
							err)
						return
					}

					// 3. First attempt fails.
					_, err = store.FailAttempt(
						ctx, pd.hash,
						pd.attempts[0].AttemptID,
						&HTLCFailInfo{
							FailTime: time.Now(),
							Reason:   HTLCFailUnreadable,
						},
					)
					if err != nil {
						b.Errorf("FailAttempt: %v", err)
						return
					}

					// 4. Fetch payment state.
					_, err = store.FetchPayment(
						ctx, pd.hash,
					)
					if err != nil {
						b.Errorf("FetchPayment: %v", err)
						return
					}

					// 5. Register second attempt.
					_, err = store.RegisterAttempt(
						ctx, pd.hash, pd.attempts[1],
					)
					if err != nil {
						b.Errorf("RegisterAttempt: %v",
							err)
						return
					}

					// 6. Settle second attempt.
					_, err = store.SettleAttempt(
						ctx, pd.hash,
						pd.attempts[1].AttemptID,
						&HTLCSettleInfo{
							Preimage:   pd.preimage,
							SettleTime: time.Now(),
						},
					)
					if err != nil {
						b.Errorf("SettleAttempt: %v",
							err)
						return
					}

					// 7. Cleanup failed attempts.
					err = store.DeleteFailedAttempts(
						ctx, pd.hash,
					)
					if err != nil {
						b.Errorf("DeleteFailed: %v",
							err)
						return
					}
				}
			})
		})
	}
}
