package batch

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// batchTestIntervals is a list of batch commit intervals to use for
// benchmarking tests.
var batchTestIntervals = []time.Duration{
	time.Millisecond * 0,
	time.Millisecond * 50,
	time.Millisecond * 100,
	time.Millisecond * 200,
	time.Millisecond * 500,
}

// TestRetry tests the retry logic of the batch scheduler.
func TestRetry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dbDir := t.TempDir()

	dbName := filepath.Join(dbDir, "weks.db")
	db, err := walletdb.Create(
		"bdb", dbName, true, kvdb.DefaultDBTimeout, false,
	)
	if err != nil {
		t.Fatalf("unable to create walletdb: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
	})

	var (
		mu     sync.Mutex
		called int
	)
	sched := NewTimeScheduler[kvdb.RwTx](
		NewBoltBackend[kvdb.RwTx](db), &mu, time.Second,
	)

	// First, we construct a request that should retry individually and
	// execute it non-lazily. It should still return the error the second
	// time.
	req := &Request[kvdb.RwTx]{
		Do: func(tx kvdb.RwTx) error {
			called++

			return errors.New("test")
		},
	}
	err = sched.Execute(ctx, req)

	// Check and reset the called counter.
	mu.Lock()
	require.Equal(t, 2, called)
	called = 0
	mu.Unlock()

	require.ErrorContains(t, err, "test")

	// Now, we construct a request that should NOT retry because it returns
	// a serialization error, which should cause the underlying postgres
	// transaction to retry. Since we aren't using postgres, this will
	// cause the transaction to not be retried at all.
	req = &Request[kvdb.RwTx]{
		Do: func(tx kvdb.RwTx) error {
			called++

			return errors.New("could not serialize access")
		},
	}
	err = sched.Execute(ctx, req)

	// Check the called counter.
	mu.Lock()
	require.Equal(t, 1, called)
	mu.Unlock()

	require.ErrorContains(t, err, "could not serialize access")
}

// TestReadOnly just ensures that nothing breaks if we specify a read-only tx
// and then continue to add a write transaction to the same batch.
func TestReadOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("bbolt-ReadWrite", func(t *testing.T) {
		db, err := walletdb.Create(
			"bdb", filepath.Join(t.TempDir(), "weks.db"), true,
			kvdb.DefaultDBTimeout, false,
		)
		require.NoError(t, err)
		if err != nil {
			t.Fatalf("unable to create walletdb: %v", err)
		}
		t.Cleanup(func() {
			require.NoError(t, db.Close())
		})

		// Create a bbolt read-write scheduler.
		rwSche := NewTimeScheduler[kvdb.RwTx](
			NewBoltBackend[kvdb.RwTx](db), nil, time.Second,
		)

		// Call it without a read-only option.
		var called bool
		req := &Request[kvdb.RwTx]{
			Do: func(tx kvdb.RwTx) error {
				called = true
				return nil
			},
		}
		require.NoError(t, rwSche.Execute(ctx, req))
		require.True(t, called)

		// Call it with a read-only option.
		called = false
		req = &Request[kvdb.RwTx]{
			Opts: NewSchedulerOptions(ReadOnly()),
			Do: func(tx kvdb.RwTx) error {
				called = true
				return nil
			},
		}
		require.NoError(t, rwSche.Execute(ctx, req))
		require.True(t, called)

		// Now, spin off a bunch of reads and writes at the same time
		// so that we can simulate the upgrade from read-only to
		// read-write.
		var (
			wg       sync.WaitGroup
			reads    = 0
			readsMu  sync.Mutex
			writes   = 0
			writesMu sync.Mutex
		)
		for i := 0; i < 100; i++ {
			// Spin off the reads.
			wg.Add(1)
			go func() {
				defer wg.Done()

				req := &Request[kvdb.RwTx]{
					Opts: NewSchedulerOptions(ReadOnly()),
					Do: func(tx kvdb.RwTx) error {
						readsMu.Lock()
						reads++
						readsMu.Unlock()

						return nil
					},
				}
				require.NoError(t, rwSche.Execute(ctx, req))
			}()

			// Spin off the writes.
			wg.Add(1)
			go func() {
				defer wg.Done()

				req := &Request[kvdb.RwTx]{
					Do: func(tx kvdb.RwTx) error {
						writesMu.Lock()
						writes++
						writesMu.Unlock()

						return nil
					},
				}
				require.NoError(t, rwSche.Execute(ctx, req))
			}()
		}

		wg.Wait()
		require.Equal(t, 100, reads)
		require.Equal(t, 100, writes)
	})

	// Note that if the scheduler is initialized with a read-only bbolt tx,
	// then the ReadOnly option does nothing as it will be read-only
	// regardless.
	t.Run("bbolt-ReadOnly", func(t *testing.T) {
		db, err := walletdb.Create(
			"bdb", filepath.Join(t.TempDir(), "weks.db"), true,
			kvdb.DefaultDBTimeout, false,
		)
		require.NoError(t, err)
		if err != nil {
			t.Fatalf("unable to create walletdb: %v", err)
		}
		t.Cleanup(func() {
			require.NoError(t, db.Close())
		})

		// Create a bbolt read only scheduler.
		rwSche := NewTimeScheduler[kvdb.RTx](
			NewBoltBackend[kvdb.RTx](db), nil, time.Second,
		)

		// Call it without a read-only option.
		var called bool
		req := &Request[kvdb.RTx]{
			Do: func(tx kvdb.RTx) error {
				called = true
				return nil
			},
		}
		require.NoError(t, rwSche.Execute(ctx, req))
		require.True(t, called)

		// Call it with a read-only option.
		called = false
		req = &Request[kvdb.RTx]{
			Opts: NewSchedulerOptions(ReadOnly()),
			Do: func(tx kvdb.RTx) error {
				called = true
				return nil
			},
		}
		require.NoError(t, rwSche.Execute(ctx, req))
		require.True(t, called)
	})

	t.Run("sql", func(t *testing.T) {
		base := sqldb.NewTestSqliteDB(t).BaseDB
		db := sqldb.NewTransactionExecutor(
			base, func(tx *sql.Tx) *sqlc.Queries {
				return base.WithTx(tx)
			},
		)

		// Create a SQL scheduler with a long batch interval.
		scheduler := NewTimeScheduler[*sqlc.Queries](
			db, nil, time.Second,
		)

		// writeRecord is a helper that adds a single new invoice to the
		// database. It uses the 'i' argument to create a unique hash
		// for the invoice.
		writeRecord := func(t *testing.T, tx *sqlc.Queries, i int64) {
			var hash [8]byte
			binary.BigEndian.PutUint64(hash[:], uint64(i))

			_, err := tx.InsertInvoice(
				ctx, sqlc.InsertInvoiceParams{
					Hash:               hash[:],
					PaymentAddr:        hash[:],
					PaymentRequestHash: hash[:],
					Expiry:             -123,
				},
			)
			require.NoError(t, err)
		}

		// readRecord is a helper that reads a single invoice from the
		// database. It uses the 'i' argument to create a unique hash
		// for the invoice.
		readRecord := func(t *testing.T, tx *sqlc.Queries,
			i int) error {

			var hash [8]byte
			binary.BigEndian.PutUint64(hash[:], uint64(i))

			_, err := tx.GetInvoiceByHash(ctx, hash[:])

			return err
		}

		// Execute a bunch of read-only requests in parallel. These
		// should be batched together and kept as read only.
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				req := &Request[*sqlc.Queries]{
					Opts: NewSchedulerOptions(ReadOnly()),
					Do: func(tx *sqlc.Queries) error {
						err := readRecord(t, tx, i)
						require.ErrorIs(
							t, err, sql.ErrNoRows,
						)

						return nil
					},
				}
				require.NoError(t, scheduler.Execute(ctx, req))
			}(i)
		}
		wg.Wait()

		// Now, execute reads and writes in parallel. These should be
		// batched together and the tx should be updated to read-write.
		// We just simulate this scenario. Write transactions succeeding
		// are how we know that the tx was upgraded to read-write.
		for i := 0; i < 100; i++ {
			// Spin off the writes.
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				req := &Request[*sqlc.Queries]{
					Do: func(tx *sqlc.Queries) error {
						writeRecord(t, tx, int64(i))

						return nil
					},
				}
				require.NoError(t, scheduler.Execute(ctx, req))
			}(i)

			// Spin off the reads.
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				errExpected := func(err error) {
					noRows := errors.Is(err, sql.ErrNoRows)
					require.True(t, err == nil || noRows)
				}

				req := &Request[*sqlc.Queries]{
					Opts: NewSchedulerOptions(ReadOnly()),
					Do: func(tx *sqlc.Queries) error {
						err := readRecord(t, tx, i)
						errExpected(err)

						return nil
					},
				}
				require.NoError(t, scheduler.Execute(ctx, req))
			}(i)
		}
		wg.Wait()
	})
}

// BenchmarkBoltBatching benchmarks the performance of the batch scheduler
// against the bolt backend.
func BenchmarkBoltBatching(b *testing.B) {
	setUpDB := func(b *testing.B) kvdb.Backend {
		// Create a new database backend for the test.
		backend, backendCleanup, err := kvdb.GetTestBackend(
			b.TempDir(), "db",
		)
		require.NoError(b, err)
		b.Cleanup(func() { backendCleanup() })

		return backend
	}

	// writeRecord is a helper that writes a simple record to the
	// database. It creates a top-level bucket and a sub-bucket, then
	// writes a record with a sequence number as the key.
	writeRecord := func(b *testing.B, tx kvdb.RwTx) {
		bucket, err := tx.CreateTopLevelBucket([]byte("top-level"))
		require.NoError(b, err)

		subBucket, err := bucket.CreateBucketIfNotExists(
			[]byte("sub-bucket"),
		)
		require.NoError(b, err)

		seq, err := subBucket.NextSequence()
		require.NoError(b, err)

		var key [8]byte
		binary.BigEndian.PutUint64(key[:], seq)

		err = subBucket.Put(key[:], []byte("value"))
		require.NoError(b, err)
	}

	// verifyRecordsWritten is a helper that verifies that the writeRecord
	// helper was called the expected number of times.
	verifyRecordsWritten := func(b *testing.B, db kvdb.Backend, N int) {
		err := db.View(func(tx kvdb.RTx) error {
			bucket := tx.ReadBucket([]byte("top-level"))
			require.NotNil(b, bucket)

			subBucket := bucket.NestedReadBucket(
				[]byte("sub-bucket"),
			)
			require.NotNil(b, subBucket)
			require.EqualValues(b, subBucket.Sequence(), N)

			return nil
		}, func() {})
		require.NoError(b, err)
	}

	// This test benchmarks the performance when using N new transactions
	// for N write queries. This does not use the scheduler.
	b.Run("N txs for N write queries", func(b *testing.B) {
		db := setUpDB(b)
		b.ResetTimer()

		var wg sync.WaitGroup
		for i := 0; i < b.N; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				err := db.Update(func(tx kvdb.RwTx) error {
					writeRecord(b, tx)
					return nil
				}, func() {})
				require.NoError(b, err)
			}()
		}
		wg.Wait()

		b.StopTimer()
		verifyRecordsWritten(b, db, b.N)
	})

	// This test benchmarks the performance when using a single transaction
	// for N write queries. This does not use the scheduler.
	b.Run("1 txs for N write queries", func(b *testing.B) {
		db := setUpDB(b)
		b.ResetTimer()

		err := db.Update(func(tx kvdb.RwTx) error {
			for i := 0; i < b.N; i++ {
				writeRecord(b, tx)
			}

			return nil
		}, func() {})
		require.NoError(b, err)

		b.StopTimer()
		verifyRecordsWritten(b, db, b.N)
	})

	// batchTest benches the performance of the batch scheduler configured
	// with/without the LazyAdd option and with the given commit interval.
	batchTest := func(b *testing.B, lazy bool, interval time.Duration) {
		ctx := b.Context()

		db := setUpDB(b)

		scheduler := NewTimeScheduler(
			NewBoltBackend[kvdb.RwTx](db), nil, interval,
		)

		var opts []SchedulerOption
		if lazy {
			opts = append(opts, LazyAdd())
		}

		b.ResetTimer()

		var wg sync.WaitGroup
		for i := 0; i < b.N; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				r := &Request[kvdb.RwTx]{
					Opts: NewSchedulerOptions(
						opts...,
					),
					Do: func(tx kvdb.RwTx) error {
						writeRecord(b, tx)
						return nil
					},
				}

				err := scheduler.Execute(ctx, r)
				require.NoError(b, err)
			}()
		}
		wg.Wait()

		b.StopTimer()
		verifyRecordsWritten(b, db, b.N)
	}

	for _, lazy := range []bool{true, false} {
		for _, interval := range batchTestIntervals {
			name := fmt.Sprintf(
				"batched queries %s lazy: %v", interval, lazy,
			)

			b.Run(name, func(b *testing.B) {
				batchTest(b, lazy, interval)
			})
		}
	}
}

// BenchmarkSQLBatching benchmarks the performance of the batch scheduler
// against the sqlite and postgres backends.
func BenchmarkSQLBatching(b *testing.B) {
	b.Run("sqlite", func(b *testing.B) {
		benchmarkSQLBatching(b, true)
	})

	b.Run("postgres", func(b *testing.B) {
		benchmarkSQLBatching(b, false)
	})
}

// benchmarkSQLBatching benchmarks the performance of the batch scheduler
// against an SQL backend. It uses the AddInvoice query as the operation to
// benchmark.
func benchmarkSQLBatching(b *testing.B, sqlite bool) {
	// First create a shared Postgres instance so we don't spawn a new
	// docker container for each test.
	pgFixture := sqldb.NewTestPgFixture(
		b, sqldb.DefaultPostgresFixtureLifetime,
	)
	b.Cleanup(func() {
		pgFixture.TearDown(b)
	})

	setUpDB := func(b *testing.B) sqldb.BatchedTx[*sqlc.Queries] {
		var db *sqldb.BaseDB
		if sqlite {
			db = sqldb.NewTestSqliteDB(b).BaseDB
		} else {
			db = sqldb.NewTestPostgresDB(b, pgFixture).BaseDB
		}

		return sqldb.NewTransactionExecutor(
			db, func(tx *sql.Tx) *sqlc.Queries {
				return db.WithTx(tx)
			},
		)
	}

	ctx := b.Context()
	opts := sqldb.WriteTxOpt()

	// writeRecord is a helper that adds a single new invoice to the
	// database. It uses the 'i' argument to create a unique hash for the
	// invoice.
	writeRecord := func(b *testing.B, tx *sqlc.Queries, i int64) {
		var hash [8]byte
		binary.BigEndian.PutUint64(hash[:], uint64(i))

		_, err := tx.InsertInvoice(ctx, sqlc.InsertInvoiceParams{
			Hash:               hash[:],
			PaymentAddr:        hash[:],
			PaymentRequestHash: hash[:],
			Expiry:             -123,
		})
		require.NoError(b, err)
	}

	// verifyRecordsWritten is a helper that verifies that the writeRecord
	// helper was called the expected number of times. We know that N was
	// used to derive the hash for each invoice persisted to the DB, so we
	// can use it to verify that the last expected invoice was written.
	verifyRecordsWritten := func(b *testing.B,
		tx sqldb.BatchedTx[*sqlc.Queries], N int) {

		var hash [8]byte
		binary.BigEndian.PutUint64(hash[:], uint64(N-1))

		err := tx.ExecTx(ctx, opts, func(queries *sqlc.Queries) error {
			_, err := queries.GetInvoiceByHash(ctx, hash[:])
			require.NoError(b, err)

			return nil
		}, func() {},
		)
		require.NoError(b, err)
	}

	// This test benchmarks the performance when using N new transactions
	// for N write queries. This does not use the scheduler.
	b.Run("N txs for N write queries", func(b *testing.B) {
		db := setUpDB(b)
		b.ResetTimer()

		var wg sync.WaitGroup
		for i := 0; i < b.N; i++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()

				err := db.ExecTx(
					ctx, opts,
					func(tx *sqlc.Queries) error {
						writeRecord(b, tx, int64(j))
						return nil
					}, func() {},
				)
				require.NoError(b, err)
			}(i)
		}
		wg.Wait()

		b.StopTimer()
		verifyRecordsWritten(b, db, b.N)
	})

	// This test benchmarks the performance when using a single transaction
	// for N write queries. This does not use the scheduler.
	b.Run("1 txs for N write queries", func(b *testing.B) {
		db := setUpDB(b)
		b.ResetTimer()

		err := db.ExecTx(
			ctx, opts,
			func(tx *sqlc.Queries) error {
				for i := 0; i < b.N; i++ {
					writeRecord(b, tx, int64(i))
				}

				return nil
			}, func() {},
		)
		require.NoError(b, err)

		b.StopTimer()
		verifyRecordsWritten(b, db, b.N)
	})

	// batchTest benches the performance of the batch scheduler configured
	// with/without the LazyAdd option and with the given commit interval.
	batchTest := func(b *testing.B, lazy bool, interval time.Duration) {
		db := setUpDB(b)

		scheduler := NewTimeScheduler[*sqlc.Queries](
			db, nil, interval,
		)

		var opts []SchedulerOption
		if lazy {
			opts = append(opts, LazyAdd())
		}

		b.ResetTimer()

		var wg sync.WaitGroup
		for i := 0; i < b.N; i++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()

				r := &Request[*sqlc.Queries]{
					Opts: NewSchedulerOptions(
						opts...,
					),
					Do: func(tx *sqlc.Queries) error {
						writeRecord(b, tx, int64(j))
						return nil
					},
				}

				err := scheduler.Execute(ctx, r)
				require.NoError(b, err)
			}(i)
		}
		wg.Wait()

		b.StopTimer()
		verifyRecordsWritten(b, db, b.N)
	}

	for _, lazy := range []bool{true, false} {
		for _, interval := range batchTestIntervals {
			name := fmt.Sprintf(
				"batched queries %s lazy: %v", interval, lazy,
			)

			b.Run(name, func(b *testing.B) {
				batchTest(b, lazy, interval)
			})
		}
	}
}
