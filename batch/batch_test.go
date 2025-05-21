package batch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// TestRetry tests the retry logic of the batch scheduler.
func TestRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

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
		Update: func(tx kvdb.RwTx) error {
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
		Update: func(tx kvdb.RwTx) error {
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
		ctx := context.Background()

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
					Update: func(tx kvdb.RwTx) error {
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

	batchTestIntervals := []time.Duration{
		time.Millisecond * 100,
		time.Millisecond * 200,
		time.Millisecond * 500,
		time.Millisecond * 700,
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
