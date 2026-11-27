package kvdb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

func TestBolt(t *testing.T) {
	tests := []struct {
		name string
		test func(*testing.T, walletdb.DB)
	}{
		{
			name: "read cursor empty interval",
			test: testReadCursorEmptyInterval,
		},
		{
			name: "read cursor non empty interval",
			test: testReadCursorNonEmptyInterval,
		},
		{
			name: "read write cursor",
			test: testReadWriteCursor,
		},
		{
			name: "read write cursor with bucket and value",
			test: testReadWriteCursorWithBucketAndValue,
		},
		{
			name: "bucket creation",
			test: testBucketCreation,
		},
		{
			name: "bucket deletion",
			test: testBucketDeletion,
		},
		{
			name: "bucket for each",
			test: testBucketForEach,
		},
		{
			name: "bucket for each with error",
			test: testBucketForEachWithError,
		},
		{
			name: "bucket sequence",
			test: testBucketSequence,
		},
		{
			name: "key clash",
			test: testKeyClash,
		},
		{
			name: "bucket create delete",
			test: testBucketCreateDelete,
		},
		{
			name: "bucket lookup after delete",
			test: testBucketLookupAfterDelete,
		},
		{
			name: "bucket lookup after create",
			test: testBucketLookupAfterCreate,
		},
		{
			name: "bucket lookup across txns",
			test: testBucketLookupAcrossTxns,
		},
		{
			name: "tx manual commit",
			test: testTxManualCommit,
		},
		{
			name: "tx rollback",
			test: testTxRollback,
		},
		{
			name: "prefetch",
			test: testPrefetch,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := NewBoltFixture(t)

			test.test(t, f.NewBackend())
		})
	}
}

// TestBoltAutoCompact asserts that a bolt database that is compacted when
// being opened still has all of its content afterwards.
func TestBoltAutoCompact(t *testing.T) {
	t.Parallel()

	const (
		numBuckets = 5
		numKeys    = 2000
	)

	var (
		nested = []byte("nested")
		value  = make([]byte, 200)
	)
	key := func(i int) []byte {
		var k [4]byte
		binary.BigEndian.PutUint32(k[:], uint32(i))

		return k[:]
	}

	cfg := &BoltBackendConfig{
		DBPath:         t.TempDir(),
		DBFileName:     "test.db",
		NoFreelistSync: true,
		DBTimeout:      DefaultDBTimeout,
	}
	dbFilePath := filepath.Join(cfg.DBPath, cfg.DBFileName)

	// Create a database with a number of buckets, each of which also has
	// a nested bucket.
	db, err := GetBoltBackend(cfg)
	require.NoError(t, err)

	err = Update(db, func(tx RwTx) error {
		for i := range numBuckets {
			bucket, err := tx.CreateTopLevelBucket(key(i))
			if err != nil {
				return err
			}
			if err := bucket.SetSequence(uint64(i)); err != nil {
				return err
			}

			sub, err := bucket.CreateBucket(nested)
			if err != nil {
				return err
			}

			for j := range numKeys {
				err := bucket.Put(key(j), value)
				if err != nil {
					return err
				}

				err = sub.Put(key(j), key(i))
				if err != nil {
					return err
				}
			}
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	// Delete every other bucket so that there is something to compact.
	err = Update(db, func(tx RwTx) error {
		for i := 0; i < numBuckets; i += 2 {
			if err := tx.DeleteTopLevelBucket(key(i)); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	sizeBefore, err := os.Stat(dbFilePath)
	require.NoError(t, err)

	// Opening the database again compacts it first.
	cfg.AutoCompact = true
	db, err = GetBoltBackend(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	sizeAfter, err := os.Stat(dbFilePath)
	require.NoError(t, err)
	require.Less(t, sizeAfter.Size(), sizeBefore.Size())

	// The temporary file must be gone and the compaction be recorded.
	require.NoFileExists(
		t, filepath.Join(cfg.DBPath, DefaultTempDBFileName),
	)
	require.FileExists(t, dbFilePath+LastCompactionFileNameSuffix)

	err = View(db, func(tx RTx) error {
		for i := range numBuckets {
			bucket := tx.ReadBucket(key(i))
			if i%2 == 0 {
				require.Nil(t, bucket)
				continue
			}
			require.NotNil(t, bucket)

			sub := bucket.NestedReadBucket(nested)
			require.NotNil(t, sub)

			for j := range numKeys {
				require.Equal(t, value, bucket.Get(key(j)))
				require.Equal(t, key(i), sub.Get(key(j)))
			}
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	// Sequences are only exposed by read-write buckets.
	err = Update(db, func(tx RwTx) error {
		for i := 1; i < numBuckets; i += 2 {
			bucket := tx.ReadWriteBucket(key(i))
			require.EqualValues(t, i, bucket.Sequence())
		}

		return nil
	}, func() {})
	require.NoError(t, err)
}
