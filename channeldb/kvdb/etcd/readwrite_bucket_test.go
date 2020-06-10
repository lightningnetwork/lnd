// +build kvdb_etcd

package etcd

import (
	"fmt"
	"math"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/assert"
)

func TestBucketCreation(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		// empty bucket name
		b, err := tx.CreateTopLevelBucket(nil)
		assert.Error(t, walletdb.ErrBucketNameRequired, err)
		assert.Nil(t, b)

		// empty bucket name
		b, err = tx.CreateTopLevelBucket([]byte(""))
		assert.Error(t, walletdb.ErrBucketNameRequired, err)
		assert.Nil(t, b)

		// "apple"
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		assert.NoError(t, err)
		assert.NotNil(t, apple)

		// Check bucket tx.
		assert.Equal(t, tx, apple.Tx())

		// "apple" already created
		b, err = tx.CreateTopLevelBucket([]byte("apple"))
		assert.NoError(t, err)
		assert.NotNil(t, b)

		// "apple/banana"
		banana, err := apple.CreateBucket([]byte("banana"))
		assert.NoError(t, err)
		assert.NotNil(t, banana)

		banana, err = apple.CreateBucketIfNotExists([]byte("banana"))
		assert.NoError(t, err)
		assert.NotNil(t, banana)

		// Try creating "apple/banana" again
		b, err = apple.CreateBucket([]byte("banana"))
		assert.Error(t, walletdb.ErrBucketExists, err)
		assert.Nil(t, b)

		// "apple/mango"
		mango, err := apple.CreateBucket([]byte("mango"))
		assert.Nil(t, err)
		assert.NotNil(t, mango)

		// "apple/banana/pear"
		pear, err := banana.CreateBucket([]byte("pear"))
		assert.Nil(t, err)
		assert.NotNil(t, pear)

		// empty bucket
		assert.Nil(t, apple.NestedReadWriteBucket(nil))
		assert.Nil(t, apple.NestedReadWriteBucket([]byte("")))

		// "apple/pear" doesn't exist
		assert.Nil(t, apple.NestedReadWriteBucket([]byte("pear")))

		// "apple/banana" exits
		assert.NotNil(t, apple.NestedReadWriteBucket([]byte("banana")))
		assert.NotNil(t, apple.NestedReadBucket([]byte("banana")))
		return nil
	})

	assert.Nil(t, err)

	expected := map[string]string{
		bkey("apple"):                   bval("apple"),
		bkey("apple", "banana"):         bval("apple", "banana"),
		bkey("apple", "mango"):          bval("apple", "mango"),
		bkey("apple", "banana", "pear"): bval("apple", "banana", "pear"),
	}
	assert.Equal(t, expected, f.Dump())
}

func TestBucketDeletion(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		// "apple"
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		assert.Nil(t, err)
		assert.NotNil(t, apple)

		// "apple/banana"
		banana, err := apple.CreateBucket([]byte("banana"))
		assert.Nil(t, err)
		assert.NotNil(t, banana)

		kvs := []KV{{"key1", "val1"}, {"key2", "val2"}, {"key3", "val3"}}

		for _, kv := range kvs {
			assert.NoError(t, banana.Put([]byte(kv.key), []byte(kv.val)))
			assert.Equal(t, []byte(kv.val), banana.Get([]byte(kv.key)))
		}

		// Delete a k/v from "apple/banana"
		assert.NoError(t, banana.Delete([]byte("key2")))
		// Try getting/putting/deleting invalid k/v's.
		assert.Nil(t, banana.Get(nil))
		assert.Error(t, walletdb.ErrKeyRequired, banana.Put(nil, []byte("val")))
		assert.Error(t, walletdb.ErrKeyRequired, banana.Delete(nil))

		// Try deleting a k/v that doesn't exist.
		assert.NoError(t, banana.Delete([]byte("nokey")))

		// "apple/pear"
		pear, err := apple.CreateBucket([]byte("pear"))
		assert.Nil(t, err)
		assert.NotNil(t, pear)

		// Put some values into "apple/pear"
		for _, kv := range kvs {
			assert.Nil(t, pear.Put([]byte(kv.key), []byte(kv.val)))
			assert.Equal(t, []byte(kv.val), pear.Get([]byte(kv.key)))
		}

		// Create nested bucket "apple/pear/cherry"
		cherry, err := pear.CreateBucket([]byte("cherry"))
		assert.Nil(t, err)
		assert.NotNil(t, cherry)

		// Put some values into "apple/pear/cherry"
		for _, kv := range kvs {
			assert.NoError(t, cherry.Put([]byte(kv.key), []byte(kv.val)))
		}

		// Read back values in "apple/pear/cherry" trough a read bucket.
		cherryReadBucket := pear.NestedReadBucket([]byte("cherry"))
		for _, kv := range kvs {
			assert.Equal(
				t, []byte(kv.val),
				cherryReadBucket.Get([]byte(kv.key)),
			)
		}

		// Try deleting some invalid buckets.
		assert.Error(t,
			walletdb.ErrBucketNameRequired, apple.DeleteNestedBucket(nil),
		)

		// Try deleting a non existing bucket.
		assert.Error(
			t,
			walletdb.ErrBucketNotFound,
			apple.DeleteNestedBucket([]byte("missing")),
		)

		// Delete "apple/pear"
		assert.Nil(t, apple.DeleteNestedBucket([]byte("pear")))

		// "apple/pear" deleted
		assert.Nil(t, apple.NestedReadWriteBucket([]byte("pear")))

		// "apple/pear/cherry" deleted
		assert.Nil(t, pear.NestedReadWriteBucket([]byte("cherry")))

		// Values deleted too.
		for _, kv := range kvs {
			assert.Nil(t, pear.Get([]byte(kv.key)))
			assert.Nil(t, cherry.Get([]byte(kv.key)))
		}

		// "aple/banana" exists
		assert.NotNil(t, apple.NestedReadWriteBucket([]byte("banana")))
		return nil
	})

	assert.Nil(t, err)

	expected := map[string]string{
		bkey("apple"):                   bval("apple"),
		bkey("apple", "banana"):         bval("apple", "banana"),
		vkey("key1", "apple", "banana"): "val1",
		vkey("key3", "apple", "banana"): "val3",
	}
	assert.Equal(t, expected, f.Dump())
}

func TestBucketForEach(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		// "apple"
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		assert.Nil(t, err)
		assert.NotNil(t, apple)

		// "apple/banana"
		banana, err := apple.CreateBucket([]byte("banana"))
		assert.Nil(t, err)
		assert.NotNil(t, banana)

		kvs := []KV{{"key1", "val1"}, {"key2", "val2"}, {"key3", "val3"}}

		// put some values into "apple" and "apple/banana" too
		for _, kv := range kvs {
			assert.Nil(t, apple.Put([]byte(kv.key), []byte(kv.val)))
			assert.Equal(t, []byte(kv.val), apple.Get([]byte(kv.key)))

			assert.Nil(t, banana.Put([]byte(kv.key), []byte(kv.val)))
			assert.Equal(t, []byte(kv.val), banana.Get([]byte(kv.key)))
		}

		got := make(map[string]string)
		err = apple.ForEach(func(key, val []byte) error {
			got[string(key)] = string(val)
			return nil
		})

		expected := map[string]string{
			"key1":   "val1",
			"key2":   "val2",
			"key3":   "val3",
			"banana": "",
		}

		assert.NoError(t, err)
		assert.Equal(t, expected, got)

		got = make(map[string]string)
		err = banana.ForEach(func(key, val []byte) error {
			got[string(key)] = string(val)
			return nil
		})

		assert.NoError(t, err)
		// remove the sub-bucket key
		delete(expected, "banana")
		assert.Equal(t, expected, got)

		return nil
	})

	assert.Nil(t, err)

	expected := map[string]string{
		bkey("apple"):                   bval("apple"),
		bkey("apple", "banana"):         bval("apple", "banana"),
		vkey("key1", "apple"):           "val1",
		vkey("key2", "apple"):           "val2",
		vkey("key3", "apple"):           "val3",
		vkey("key1", "apple", "banana"): "val1",
		vkey("key2", "apple", "banana"): "val2",
		vkey("key3", "apple", "banana"): "val3",
	}
	assert.Equal(t, expected, f.Dump())
}

func TestBucketForEachWithError(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		// "apple"
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		assert.Nil(t, err)
		assert.NotNil(t, apple)

		// "apple/banana"
		banana, err := apple.CreateBucket([]byte("banana"))
		assert.Nil(t, err)
		assert.NotNil(t, banana)

		// "apple/pear"
		pear, err := apple.CreateBucket([]byte("pear"))
		assert.Nil(t, err)
		assert.NotNil(t, pear)

		kvs := []KV{{"key1", "val1"}, {"key2", "val2"}}

		// Put some values into "apple" and "apple/banana" too.
		for _, kv := range kvs {
			assert.Nil(t, apple.Put([]byte(kv.key), []byte(kv.val)))
			assert.Equal(t, []byte(kv.val), apple.Get([]byte(kv.key)))
		}

		got := make(map[string]string)
		i := 0
		// Error while iterating value keys.
		err = apple.ForEach(func(key, val []byte) error {
			if i == 1 {
				return fmt.Errorf("error")
			}

			got[string(key)] = string(val)
			i++
			return nil
		})

		expected := map[string]string{
			"key1": "val1",
		}

		assert.Equal(t, expected, got)
		assert.Error(t, err)

		got = make(map[string]string)
		i = 0
		// Erro while iterating buckets.
		err = apple.ForEach(func(key, val []byte) error {
			if i == 3 {
				return fmt.Errorf("error")
			}

			got[string(key)] = string(val)
			i++
			return nil
		})

		expected = map[string]string{
			"key1":   "val1",
			"key2":   "val2",
			"banana": "",
		}

		assert.Equal(t, expected, got)
		assert.Error(t, err)
		return nil
	})

	assert.Nil(t, err)

	expected := map[string]string{
		bkey("apple"):           bval("apple"),
		bkey("apple", "banana"): bval("apple", "banana"),
		bkey("apple", "pear"):   bval("apple", "pear"),
		vkey("key1", "apple"):   "val1",
		vkey("key2", "apple"):   "val2",
	}
	assert.Equal(t, expected, f.Dump())
}

func TestBucketSequence(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		assert.Nil(t, err)
		assert.NotNil(t, apple)

		banana, err := apple.CreateBucket([]byte("banana"))
		assert.Nil(t, err)
		assert.NotNil(t, banana)

		assert.Equal(t, uint64(0), apple.Sequence())
		assert.Equal(t, uint64(0), banana.Sequence())

		assert.Nil(t, apple.SetSequence(math.MaxUint64))
		assert.Equal(t, uint64(math.MaxUint64), apple.Sequence())

		for i := uint64(0); i < uint64(5); i++ {
			s, err := apple.NextSequence()
			assert.Nil(t, err)
			assert.Equal(t, i, s)
		}

		return nil
	})

	assert.Nil(t, err)
}
