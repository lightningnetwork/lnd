package kvdb

import (
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

// TestBoltMmapPopulate asserts that a bolt backend can be created, written to
// and opened again with the mmap populate option set.
func TestBoltMmapPopulate(t *testing.T) {
	t.Parallel()

	var (
		bucket = []byte("bucket")
		key    = []byte("key")
		value  = []byte("value")
	)

	cfg := &BoltBackendConfig{
		DBPath:         t.TempDir(),
		DBFileName:     "test.db",
		NoFreelistSync: true,
		DBTimeout:      DefaultDBTimeout,
		MmapPopulate:   true,
	}

	// The first call creates the database file.
	db, err := GetBoltBackend(cfg)
	require.NoError(t, err)

	err = Update(db, func(tx RwTx) error {
		b, err := tx.CreateTopLevelBucket(bucket)
		if err != nil {
			return err
		}

		return b.Put(key, value)
	}, func() {})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// The second call opens the existing database file.
	db, err = GetBoltBackend(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	err = View(db, func(tx RTx) error {
		require.Equal(t, value, tx.ReadBucket(bucket).Get(key))

		return nil
	}, func() {})
	require.NoError(t, err)
}
