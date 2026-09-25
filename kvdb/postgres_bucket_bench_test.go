//go:build kvdb_postgres

package kvdb

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/stretchr/testify/require"
)

// numBenchKeys is the number of keys stored in the innermost bucket of the
// nested bucket benchmark.
const numBenchKeys = 100

// BenchmarkPostgresNestedBucketLookup benchmarks reading values through a
// nested bucket path that is resolved again for every read within a single
// transaction. This is the access pattern of btcwallet's address manager,
// which resolves its scope bucket for every address and account it reads.
func BenchmarkPostgresNestedBucketLookup(b *testing.B) {
	// Start embedded postgres instance for benchmarks.
	stop, err := postgres.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		if err := stop(); err != nil {
			b.Logf("Failed to stop postgres: %v", err)
		}
	})

	depths := []int{1, 3}
	readsPerTx := []int{1, 10, 100}

	for _, depth := range depths {
		// Create a test database with a bucket path of the given depth
		// and some keys in the innermost bucket.
		f, err := postgres.NewFixture("")
		require.NoError(b, err)

		path := make([][]byte, depth)
		for i := range path {
			path[i] = []byte(fmt.Sprintf("bucket%d", i))
		}

		err = Update(f.Db, func(tx walletdb.ReadWriteTx) error {
			bucket, err := tx.CreateTopLevelBucket(path[0])
			if err != nil {
				return err
			}
			for _, key := range path[1:] {
				bucket, err = bucket.CreateBucket(key)
				if err != nil {
					return err
				}
			}

			for i := 0; i < numBenchKeys; i++ {
				key := fmt.Sprintf("key%08d", i)
				val := fmt.Sprintf("value%08d", i)
				err := bucket.Put([]byte(key), []byte(val))
				if err != nil {
					return err
				}
			}

			return nil
		}, func() {})
		require.NoError(b, err)

		for _, reads := range readsPerTx {
			read := func(tx walletdb.ReadTx) error {
				return readNested(tx, path, reads)
			}

			name := fmt.Sprintf("depth=%d/reads=%d", depth, reads)
			b.Run(name, func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					err := View(f.Db, read, func() {})
					require.NoError(b, err)
				}
			})
		}
	}
}

// readNested reads the given number of keys from the bucket at the given
// path, resolving the path again for each of them.
func readNested(tx walletdb.ReadTx, path [][]byte, reads int) error {
	for i := 0; i < reads; i++ {
		bucket := tx.ReadBucket(path[0])
		for _, key := range path[1:] {
			bucket = bucket.NestedReadBucket(key)
		}

		key := fmt.Sprintf("key%08d", i%numBenchKeys)
		if bucket.Get([]byte(key)) == nil {
			return fmt.Errorf("key %s not found", key)
		}
	}

	return nil
}
