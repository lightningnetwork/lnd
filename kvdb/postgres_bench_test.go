//go:build kvdb_postgres

package kvdb

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/stretchr/testify/require"
)

// BenchmarkPostgresCursorDelete benchmarks the cursor Delete operation with
// various dataset sizes.
func BenchmarkPostgresCursorDelete(b *testing.B) {
	// Start embedded postgres instance for benchmarks.
	stop, err := postgres.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		if err := stop(); err != nil {
			b.Logf("Failed to stop postgres: %v", err)
		}
	})

	sizes := []int{10, 100, 1000, 10000, 100000, 1000000}

	for _, size := range sizes {
		// Skip larger sizes on short benchmarks.
		if testing.Short() && size > 10000 {
			b.Skip("Skipping large dataset in short mode")
		}

		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			// Create a test database.
			f, err := postgres.NewFixture("")
			require.NoError(b, err)

			// Pre-populate the database with test data.
			b.Logf("Populating database with %d keys...", size)
			err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
				bucket, err := tx.CreateTopLevelBucket([]byte("bench"))
				if err != nil {
					return err
				}

				// Insert test data in batches for efficiency.
				batchSize := 1000
				for i := 0; i < size; i++ {
					key := fmt.Sprintf("key%08d", i)
					val := fmt.Sprintf("value%08d", i)
					if err := bucket.Put([]byte(key), []byte(val)); err != nil {
						return err
					}

					// Commit batch periodically for large datasets.
					if i > 0 && i%batchSize == 0 && size > 10000 {
						return nil
					}
				}
				return nil
			}, func() {})
			require.NoError(b, err)

			// For large datasets, commit remaining data.
			if size > 10000 {
				for i := (size / 1000) * 1000; i < size; i++ {
					err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
						bucket := tx.ReadWriteBucket([]byte("bench"))
						key := fmt.Sprintf("key%08d", i)
						val := fmt.Sprintf("value%08d", i)
						return bucket.Put([]byte(key), []byte(val))
					}, func() {})
					require.NoError(b, err)
				}
			}

			b.ResetTimer()

			// Benchmark the delete operations.
			for i := 0; i < b.N; i++ {
				// Delete using cursor.
				err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
					bucket := tx.ReadWriteBucket([]byte("bench"))
					cursor := bucket.ReadWriteCursor()

					// Position cursor and delete.
					idx := i % size
					key := fmt.Sprintf("key%08d", idx)
					k, _ := cursor.Seek([]byte(key))
					if k != nil {
						return cursor.Delete()
					}
					return nil
				}, func() {})
				require.NoError(b, err)

				// Re-add the deleted key for next iteration.
				b.StopTimer()
				err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
					bucket := tx.ReadWriteBucket([]byte("bench"))
					idx := i % size
					key := fmt.Sprintf("key%08d", idx)
					val := fmt.Sprintf("value%08d", idx)
					return bucket.Put([]byte(key), []byte(val))
				}, func() {})
				require.NoError(b, err)
				b.StartTimer()
			}
		})
	}
}

// BenchmarkPostgresBucketDelete benchmarks the bucket Delete operation with
// various dataset sizes.
func BenchmarkPostgresBucketDelete(b *testing.B) {
	// Start embedded postgres instance for benchmarks.
	stop, err := postgres.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		if err := stop(); err != nil {
			b.Logf("Failed to stop postgres: %v", err)
		}
	})

	sizes := []int{10, 100, 1000, 10000, 100000, 1000000}

	for _, size := range sizes {
		// Skip larger sizes on short benchmarks.
		if testing.Short() && size > 10000 {
			b.Skip("Skipping large dataset in short mode")
		}

		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			// Create a test database.
			f, err := postgres.NewFixture("")
			require.NoError(b, err)

			// Pre-populate the database with test data.
			b.Logf("Populating database with %d keys...", size)
			err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
				bucket, err := tx.CreateTopLevelBucket([]byte("bench"))
				if err != nil {
					return err
				}

				// Insert test data.
				for i := 0; i < size; i++ {
					key := fmt.Sprintf("key%08d", i)
					val := fmt.Sprintf("value%08d", i)
					if err := bucket.Put([]byte(key), []byte(val)); err != nil {
						return err
					}
				}
				return nil
			}, func() {})
			require.NoError(b, err)

			b.ResetTimer()

			// Benchmark the delete operations.
			for i := 0; i < b.N; i++ {
				// Delete directly on bucket.
				err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
					bucket := tx.ReadWriteBucket([]byte("bench"))

					idx := i % size
					key := fmt.Sprintf("key%08d", idx)
					return bucket.Delete([]byte(key))
				}, func() {})
				require.NoError(b, err)

				// Re-add the deleted key for next iteration.
				b.StopTimer()
				err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
					bucket := tx.ReadWriteBucket([]byte("bench"))
					idx := i % size
					key := fmt.Sprintf("key%08d", idx)
					val := fmt.Sprintf("value%08d", idx)
					return bucket.Put([]byte(key), []byte(val))
				}, func() {})
				require.NoError(b, err)
				b.StartTimer()
			}
		})
	}
}

// BenchmarkPostgresBucketDeleteNonExistent benchmarks deleting non-existent
// keys with various dataset sizes.
func BenchmarkPostgresBucketDeleteNonExistent(b *testing.B) {
	// Start embedded postgres instance for benchmarks.
	stop, err := postgres.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		if err := stop(); err != nil {
			b.Logf("Failed to stop postgres: %v", err)
		}
	})

	// Test with different dataset sizes to see if having more data affects
	// non-existent key deletion performance.
	sizes := []int{0, 1000, 100000, 1000000}

	for _, size := range sizes {
		// Skip larger sizes on short benchmarks.
		if testing.Short() && size > 10000 {
			b.Skip("Skipping large dataset in short mode")
		}

		b.Run(fmt.Sprintf("dbsize=%d", size), func(b *testing.B) {
			// Create a test database.
			f, err := postgres.NewFixture("")
			require.NoError(b, err)

			// Create bucket and optionally populate it.
			err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
				bucket, err := tx.CreateTopLevelBucket([]byte("bench"))
				if err != nil {
					return err
				}

				// Populate with existing data if requested.
				for i := 0; i < size; i++ {
					key := fmt.Sprintf("existing%08d", i)
					val := fmt.Sprintf("value%08d", i)
					if err := bucket.Put([]byte(key), []byte(val)); err != nil {
						return err
					}
				}
				return nil
			}, func() {})
			require.NoError(b, err)

			b.ResetTimer()

			// Benchmark deleting non-existent keys.
			for i := 0; i < b.N; i++ {
				err := Update(f.Db, func(tx walletdb.ReadWriteTx) error {
					bucket := tx.ReadWriteBucket([]byte("bench"))

					// Delete a non-existent key.
					key := fmt.Sprintf("nonexistent%08d", i)
					return bucket.Delete([]byte(key))
				}, func() {})
				require.NoError(b, err)
			}
		})
	}
}

// BenchmarkPostgresNextSequence benchmarks the NextSequence operation.
func BenchmarkPostgresNextSequence(b *testing.B) {
	// Start embedded postgres instance for benchmarks.
	stop, err := postgres.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		if err := stop(); err != nil {
			b.Logf("Failed to stop postgres: %v", err)
		}
	})

	b.Run("single_bucket", func(b *testing.B) {
		// Create a test database.
		f, err := postgres.NewFixture("")
		require.NoError(b, err)

		// Create a single bucket for benchmarking.
		err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
			top, err := tx.CreateTopLevelBucket([]byte("bench"))
			if err != nil {
				return err
			}
			_, err = top.CreateBucket([]byte("subbucket"))
			return err
		}, func() {})
		require.NoError(b, err)

		b.ResetTimer()

		// Benchmark NextSequence calls on a single bucket.
		for i := 0; i < b.N; i++ {
			err := Update(f.Db, func(tx walletdb.ReadWriteTx) error {
				top := tx.ReadWriteBucket([]byte("bench"))
				bucket := top.NestedReadWriteBucket([]byte("subbucket"))

				seq, err := bucket.NextSequence()
				if err != nil {
					return err
				}

				// Verify sequence is correct.
				expectedSeq := uint64(i + 1)
				if seq != expectedSeq {
					b.Errorf("Expected sequence %d, got %d", expectedSeq, seq)
				}

				return nil
			}, func() {})
			require.NoError(b, err)
		}
	})

	b.Run("concurrent_buckets", func(b *testing.B) {
		// Create a test database.
		f, err := postgres.NewFixture("")
		require.NoError(b, err)

		// Create multiple buckets to simulate concurrent access patterns.
		numBuckets := 10
		err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
			top, err := tx.CreateTopLevelBucket([]byte("bench"))
			if err != nil {
				return err
			}
			for i := 0; i < numBuckets; i++ {
				bucketName := fmt.Sprintf("bucket%d", i)
				_, err = top.CreateBucket([]byte(bucketName))
				if err != nil {
					return err
				}
			}
			return nil
		}, func() {})
		require.NoError(b, err)

		b.ResetTimer()

		// Benchmark NextSequence calls rotating through buckets.
		for i := 0; i < b.N; i++ {
			bucketIdx := i % numBuckets
			err := Update(f.Db, func(tx walletdb.ReadWriteTx) error {
				top := tx.ReadWriteBucket([]byte("bench"))
				bucketName := fmt.Sprintf("bucket%d", bucketIdx)
				bucket := top.NestedReadWriteBucket([]byte(bucketName))

				_, err := bucket.NextSequence()
				return err
			}, func() {})
			require.NoError(b, err)
		}
	})
}

