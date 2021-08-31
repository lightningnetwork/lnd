package kvdb

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

func testCacheFill(t *testing.T, db Backend) {
	data := map[string]interface{}{
		"apple": map[string]interface{}{
			"a1": "av1",
			"a2": "av2",
			"banana": map[string]interface{}{
				"ab1": "abv1",
			},
		},
		"banana": map[string]interface{}{
			"b1": "bv1",
		},
		"coconut": map[string]interface{}{
			"c1": "cv1",
		},
	}

	require.NoError(t, FillDB(db, data))

	topLevelBuckets := [][]byte{[]byte("apple"), []byte("banana")}
	// Skipping bucket with the name banana.
	skippedKeys := [][]byte{[]byte("banana")}

	cache := NewCache(db, topLevelBuckets, skippedKeys)
	require.NoError(t, cache.Init())

	// Update the banana buckets so we can ensure the cache will fetch all
	// values from the DB.
	Update(cache, func(tx walletdb.ReadWriteTx) error {
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		require.NoError(t, err)

		// Create new rw bucket inside a cached bucket.
		peach, err := apple.CreateBucketIfNotExists([]byte("peach"))
		require.NoError(t, err)
		peach.Put([]byte("ap1"), []byte("apv1"))

		banana := apple.NestedReadWriteBucket([]byte("banana"))
		require.NotNil(t, banana)
		banana.Put([]byte("ab2"), []byte("abv2"))

		banana, err = tx.CreateTopLevelBucket([]byte("banana"))
		require.NoError(t, err)

		// Put a new value inisde a skipped bucket.
		banana.Put([]byte("b2"), []byte("bv2"))

		// Crate a new rw bucket inside a write through bucket.
		pear, err := banana.CreateBucketIfNotExists([]byte("pear"))
		require.NoError(t, err)
		pear.Put([]byte("bp1"), []byte("bpv1"))

		return nil
	}, func() {})

	// Now read back using the kvdb interface.
	View(cache, func(tx walletdb.ReadTx) error {
		apple := tx.ReadBucket([]byte("apple"))
		require.NotNil(t, apple)

		peach := apple.NestedReadBucket([]byte("peach"))
		require.NotNil(t, peach)
		require.Equal(t, []byte("apv1"), peach.Get([]byte("ap1")))

		banana := apple.NestedReadBucket([]byte("banana"))
		require.NotNil(t, banana)
		require.Equal(t, []byte("abv2"), banana.Get([]byte("ab2")))

		banana = tx.ReadBucket([]byte("banana"))
		require.NotNil(t, banana)

		require.Equal(t, []byte("bv2"), banana.Get([]byte("b2")))

		pear := banana.NestedReadBucket([]byte("pear"))
		require.NotNil(t, pear)
		require.Equal(t, []byte("bpv1"), pear.Get([]byte("bp1")))

		return nil
	}, func() {})

	expected := map[string]interface{}{
		"apple": map[string]interface{}{
			"a1": "av1",
			"a2": "av2",
			"banana": map[string]interface{}{
				"ab1": "abv1",
				"ab2": "abv2",
			},
			"peach": map[string]interface{}{
				"ap1": "apv1",
			},
		},
		"banana": map[string]interface{}{
			"b1": "bv1",
			"b2": "bv2",
			"pear": map[string]interface{}{
				"bp1": "bpv1",
			},
		},
	}

	// Verify that both the cache and the DB has all data
	// we expect.
	require.NoError(t, VerifyDB(cache, expected))
	require.NoError(t, VerifyDB(db, expected))

	// Now wipe all data.
	cache.Wipe()
	empty := make(map[string]interface{})
	require.NoError(t, VerifyDB(cache, empty))

	// We still expect everything in the DB.
	require.NoError(t, VerifyDB(db, expected))
	require.NoError(t, cache.Close())
}

func testCacheRollback(t *testing.T, db Backend) {
	data := map[string]interface{}{
		"apple": map[string]interface{}{
			"a1": "av1",
			"a2": "av2",
			"banana": map[string]interface{}{
				"ab1": "abv1",
			},
		},
	}
	require.NoError(t, FillDB(db, data))

	cache := NewCache(db, [][]byte{[]byte("apple")}, nil)
	require.NoError(t, cache.Init())
	require.NoError(t, VerifyDB(cache, data))

	update := func(tx RwTx) error {
		coconut, err := tx.CreateTopLevelBucket([]byte("coconut"))
		require.NoError(t, err)
		coconut.Put([]byte("key"), []byte("val"))

		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		require.NoError(t, err)

		// Add a new key.
		apple.Put([]byte("key"), []byte("val"))

		// Delete an existing key.
		apple.Delete([]byte("a1"))

		// Update an existing key.
		apple.Put([]byte("a2"), []byte("new"))

		banana := apple.NestedReadWriteBucket([]byte("banana"))
		require.NotNil(t, banana)

		banana.Delete([]byte("ab1"))
		banana.Put([]byte("ab2"), []byte("abv2"))

		ab1, err := banana.CreateBucket([]byte("ab1"))
		require.NoError(t, err)
		ab1.Put([]byte("key"), []byte("val"))

		nested, err := banana.CreateBucket([]byte("nested"))
		require.NoError(t, err)

		nested.Put([]byte("n1"), []byte("nv1"))

		apple.DeleteNestedBucket([]byte("banana"))
		tx.DeleteTopLevelBucket([]byte("apple"))

		return nil
	}

	// Check rollback with manual txn.
	tx, err := cache.BeginReadWriteTx()
	require.NoError(t, err)
	update(tx)
	require.NoError(t, tx.Rollback())

	require.NoError(t, VerifyDB(cache, data))
	require.NoError(t, VerifyDB(db, data))

	// Check rollback with closed form txn failing.
	require.Error(t, cache.Update(func(tx RwTx) error {
		require.NoError(t, update(tx))
		return fmt.Errorf("fail")
	}, func() {}))

	require.NoError(t, VerifyDB(cache, data))
	require.NoError(t, VerifyDB(db, data))

	require.NoError(t, cache.Close())
}
