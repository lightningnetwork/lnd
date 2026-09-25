//go:build kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqlbase

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // Register the sqlite driver.
)

// TestBucketIDCacheLimit tests that the bucket id cache stops adding buckets
// once it is full, while still updating the buckets it already holds.
func TestBucketIDCacheLimit(t *testing.T) {
	t.Parallel()

	cache := newBucketIDCache()

	// Fill the cache, with one bucket recorded as not existing.
	cache.put(nil, []byte("missing"), nil)
	for i := int64(1); len(cache.ids) < maxCachedBucketIDs; i++ {
		id := i
		cache.put(nil, []byte(fmt.Sprintf("bucket%d", i)), &id)
	}

	// New buckets are no longer added.
	id := int64(-1)
	cache.put(nil, []byte("new"), &id)
	_, ok := cache.get(nil, []byte("new"))
	require.False(t, ok)
	require.Len(t, cache.ids, maxCachedBucketIDs)

	// A bucket that is already cached is still updated, so that a created
	// bucket can't be shadowed by a stale entry.
	cache.put(nil, []byte("missing"), &id)
	cachedID, ok := cache.get(nil, []byte("missing"))
	require.True(t, ok)
	require.Equal(t, &id, cachedID)

	cache.clear()
	require.Empty(t, cache.ids)
}

// TestBucketLookupCached tests that a nested bucket is only looked up in the
// database once per transaction.
func TestBucketLookupCached(t *testing.T) {
	t.Parallel()

	Init(0)

	db, err := NewSqlBackend(t.Context(), &Config{
		DriverName:      "sqlite",
		Dsn:             filepath.Join(t.TempDir(), "tmp.db"),
		TableNamePrefix: "test",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		if err != nil {
			return err
		}

		_, err = apple.CreateBucket([]byte("banana"))

		return err
	})
	require.NoError(t, err)

	tx, err := db.BeginReadWriteTx()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, tx.Rollback())
	})
	rwTx := tx.(*readWriteTx)

	apple := tx.ReadWriteBucket([]byte("apple"))
	require.NotNil(t, apple)
	require.NotNil(t, apple.NestedReadWriteBucket([]byte("banana")))

	// Remove "apple/banana" from the table behind the cache's back. As the
	// bucket was already looked up in this transaction, looking it up again
	// must not query the database and still return the bucket.
	_, err = rwTx.Exec(
		"DELETE FROM "+db.table+" WHERE key=$1 AND value IS NULL",
		[]byte("banana"),
	)
	require.NoError(t, err)

	require.NotNil(t, apple.NestedReadWriteBucket([]byte("banana")))
	require.NotNil(t, tx.ReadWriteBucket([]byte("apple")).
		NestedReadWriteBucket([]byte("banana")))

	// A bucket that wasn't looked up before is still read from the
	// database.
	_, err = rwTx.Exec(
		"INSERT INTO "+db.table+" (parent_id, key) VALUES($1, $2)",
		*apple.(*readWriteBucket).id, []byte("pear"),
	)
	require.NoError(t, err)
	require.NotNil(t, apple.NestedReadWriteBucket([]byte("pear")))
}
