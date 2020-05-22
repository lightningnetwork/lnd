// +build kvdb_etcd

package etcd

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/assert"
)

func TestReadCursorEmptyInterval(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		b, err := tx.CreateTopLevelBucket([]byte("alma"))
		assert.NoError(t, err)
		assert.NotNil(t, b)

		return nil
	})
	assert.NoError(t, err)

	err = db.View(func(tx walletdb.ReadTx) error {
		b := tx.ReadBucket([]byte("alma"))
		assert.NotNil(t, b)

		cursor := b.ReadCursor()
		k, v := cursor.First()
		assert.Nil(t, k)
		assert.Nil(t, v)

		k, v = cursor.Next()
		assert.Nil(t, k)
		assert.Nil(t, v)

		k, v = cursor.Last()
		assert.Nil(t, k)
		assert.Nil(t, v)

		k, v = cursor.Prev()
		assert.Nil(t, k)
		assert.Nil(t, v)

		return nil
	})
	assert.NoError(t, err)
}

func TestReadCursorNonEmptyInterval(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	testKeyValues := []KV{
		{"b", "1"},
		{"c", "2"},
		{"da", "3"},
		{"e", "4"},
	}

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		b, err := tx.CreateTopLevelBucket([]byte("alma"))
		assert.NoError(t, err)
		assert.NotNil(t, b)

		for _, kv := range testKeyValues {
			assert.NoError(t, b.Put([]byte(kv.key), []byte(kv.val)))
		}
		return nil
	})

	assert.NoError(t, err)

	err = db.View(func(tx walletdb.ReadTx) error {
		b := tx.ReadBucket([]byte("alma"))
		assert.NotNil(t, b)

		// Iterate from the front.
		var kvs []KV
		cursor := b.ReadCursor()
		k, v := cursor.First()

		for k != nil && v != nil {
			kvs = append(kvs, KV{string(k), string(v)})
			k, v = cursor.Next()
		}
		assert.Equal(t, testKeyValues, kvs)

		// Iterate from the back.
		kvs = []KV{}
		k, v = cursor.Last()

		for k != nil && v != nil {
			kvs = append(kvs, KV{string(k), string(v)})
			k, v = cursor.Prev()
		}
		assert.Equal(t, reverseKVs(testKeyValues), kvs)

		// Random access
		perm := []int{3, 0, 2, 1}
		for _, i := range perm {
			k, v := cursor.Seek([]byte(testKeyValues[i].key))
			assert.Equal(t, []byte(testKeyValues[i].key), k)
			assert.Equal(t, []byte(testKeyValues[i].val), v)
		}

		// Seek to nonexisting key.
		k, v = cursor.Seek(nil)
		assert.Nil(t, k)
		assert.Nil(t, v)

		k, v = cursor.Seek([]byte("x"))
		assert.Nil(t, k)
		assert.Nil(t, v)

		return nil
	})

	assert.NoError(t, err)
}

func TestReadWriteCursor(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	testKeyValues := []KV{
		{"b", "1"},
		{"c", "2"},
		{"da", "3"},
		{"e", "4"},
	}

	count := len(testKeyValues)

	// Pre-store the first half of the interval.
	assert.NoError(t, db.Update(func(tx walletdb.ReadWriteTx) error {
		b, err := tx.CreateTopLevelBucket([]byte("apple"))
		assert.NoError(t, err)
		assert.NotNil(t, b)

		for i := 0; i < count/2; i++ {
			err = b.Put(
				[]byte(testKeyValues[i].key),
				[]byte(testKeyValues[i].val),
			)
			assert.NoError(t, err)
		}
		return nil
	}))

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		b := tx.ReadWriteBucket([]byte("apple"))
		assert.NotNil(t, b)

		// Store the second half of the interval.
		for i := count / 2; i < count; i++ {
			err = b.Put(
				[]byte(testKeyValues[i].key),
				[]byte(testKeyValues[i].val),
			)
			assert.NoError(t, err)
		}

		cursor := b.ReadWriteCursor()

		// First on valid interval.
		fk, fv := cursor.First()
		assert.Equal(t, []byte("b"), fk)
		assert.Equal(t, []byte("1"), fv)

		// Prev(First()) = nil
		k, v := cursor.Prev()
		assert.Nil(t, k)
		assert.Nil(t, v)

		// Last on valid interval.
		lk, lv := cursor.Last()
		assert.Equal(t, []byte("e"), lk)
		assert.Equal(t, []byte("4"), lv)

		// Next(Last()) = nil
		k, v = cursor.Next()
		assert.Nil(t, k)
		assert.Nil(t, v)

		// Delete first item, then add an item before the
		// deleted one. Check that First/Next will "jump"
		// over the deleted item and return the new first.
		_, _ = cursor.First()
		assert.NoError(t, cursor.Delete())
		assert.NoError(t, b.Put([]byte("a"), []byte("0")))
		fk, fv = cursor.First()

		assert.Equal(t, []byte("a"), fk)
		assert.Equal(t, []byte("0"), fv)

		k, v = cursor.Next()
		assert.Equal(t, []byte("c"), k)
		assert.Equal(t, []byte("2"), v)

		// Similarly test that a new end is returned if
		// the old end is deleted first.
		_, _ = cursor.Last()
		assert.NoError(t, cursor.Delete())
		assert.NoError(t, b.Put([]byte("f"), []byte("5")))

		lk, lv = cursor.Last()
		assert.Equal(t, []byte("f"), lk)
		assert.Equal(t, []byte("5"), lv)

		k, v = cursor.Prev()
		assert.Equal(t, []byte("da"), k)
		assert.Equal(t, []byte("3"), v)

		// Overwrite k/v in the middle of the interval.
		assert.NoError(t, b.Put([]byte("c"), []byte("3")))
		k, v = cursor.Prev()
		assert.Equal(t, []byte("c"), k)
		assert.Equal(t, []byte("3"), v)

		// Insert new key/values.
		assert.NoError(t, b.Put([]byte("cx"), []byte("x")))
		assert.NoError(t, b.Put([]byte("cy"), []byte("y")))

		k, v = cursor.Next()
		assert.Equal(t, []byte("cx"), k)
		assert.Equal(t, []byte("x"), v)

		k, v = cursor.Next()
		assert.Equal(t, []byte("cy"), k)
		assert.Equal(t, []byte("y"), v)

		expected := []KV{
			{"a", "0"},
			{"c", "3"},
			{"cx", "x"},
			{"cy", "y"},
			{"da", "3"},
			{"f", "5"},
		}

		// Iterate from the front.
		var kvs []KV
		k, v = cursor.First()

		for k != nil && v != nil {
			kvs = append(kvs, KV{string(k), string(v)})
			k, v = cursor.Next()
		}
		assert.Equal(t, expected, kvs)

		// Iterate from the back.
		kvs = []KV{}
		k, v = cursor.Last()

		for k != nil && v != nil {
			kvs = append(kvs, KV{string(k), string(v)})
			k, v = cursor.Prev()
		}
		assert.Equal(t, reverseKVs(expected), kvs)

		return nil
	})

	assert.NoError(t, err)

	expected := map[string]string{
		bkey("apple"):       bval("apple"),
		vkey("a", "apple"):  "0",
		vkey("c", "apple"):  "3",
		vkey("cx", "apple"): "x",
		vkey("cy", "apple"): "y",
		vkey("da", "apple"): "3",
		vkey("f", "apple"):  "5",
	}
	assert.Equal(t, expected, f.Dump())
}
