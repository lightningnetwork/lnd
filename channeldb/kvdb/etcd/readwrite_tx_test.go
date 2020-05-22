// +build kvdb_etcd

package etcd

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/assert"
)

func TestTxManualCommit(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	tx, err := db.BeginReadWriteTx()
	assert.NoError(t, err)
	assert.NotNil(t, tx)

	committed := false

	tx.OnCommit(func() {
		committed = true
	})

	apple, err := tx.CreateTopLevelBucket([]byte("apple"))
	assert.NoError(t, err)
	assert.NotNil(t, apple)
	assert.NoError(t, apple.Put([]byte("testKey"), []byte("testVal")))

	banana, err := tx.CreateTopLevelBucket([]byte("banana"))
	assert.NoError(t, err)
	assert.NotNil(t, banana)
	assert.NoError(t, banana.Put([]byte("testKey"), []byte("testVal")))
	assert.NoError(t, tx.DeleteTopLevelBucket([]byte("banana")))

	assert.NoError(t, tx.Commit())
	assert.True(t, committed)

	expected := map[string]string{
		bkey("apple"):            bval("apple"),
		vkey("testKey", "apple"): "testVal",
	}
	assert.Equal(t, expected, f.Dump())
}

func TestTxRollback(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	tx, err := db.BeginReadWriteTx()
	assert.Nil(t, err)
	assert.NotNil(t, tx)

	apple, err := tx.CreateTopLevelBucket([]byte("apple"))
	assert.Nil(t, err)
	assert.NotNil(t, apple)

	assert.NoError(t, apple.Put([]byte("testKey"), []byte("testVal")))

	assert.NoError(t, tx.Rollback())
	assert.Error(t, walletdb.ErrTxClosed, tx.Commit())
	assert.Equal(t, map[string]string{}, f.Dump())
}

func TestChangeDuringManualTx(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	tx, err := db.BeginReadWriteTx()
	assert.Nil(t, err)
	assert.NotNil(t, tx)

	apple, err := tx.CreateTopLevelBucket([]byte("apple"))
	assert.Nil(t, err)
	assert.NotNil(t, apple)

	assert.NoError(t, apple.Put([]byte("testKey"), []byte("testVal")))

	// Try overwriting the bucket key.
	f.Put(bkey("apple"), "banana")

	// TODO: translate error
	assert.NotNil(t, tx.Commit())
	assert.Equal(t, map[string]string{
		bkey("apple"): "banana",
	}, f.Dump())
}

func TestChangeDuringUpdate(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	assert.NoError(t, err)

	count := 0

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		assert.NoError(t, err)
		assert.NotNil(t, apple)

		assert.NoError(t, apple.Put([]byte("key"), []byte("value")))

		if count == 0 {
			f.Put(vkey("key", "apple"), "new_value")
			f.Put(vkey("key2", "apple"), "value2")
		}

		cursor := apple.ReadCursor()
		k, v := cursor.First()
		assert.Equal(t, []byte("key"), k)
		assert.Equal(t, []byte("value"), v)
		assert.Equal(t, v, apple.Get([]byte("key")))

		k, v = cursor.Next()
		if count == 0 {
			assert.Nil(t, k)
			assert.Nil(t, v)
		} else {
			assert.Equal(t, []byte("key2"), k)
			assert.Equal(t, []byte("value2"), v)
		}

		count++
		return nil
	})

	assert.Nil(t, err)
	assert.Equal(t, count, 2)

	expected := map[string]string{
		bkey("apple"):         bval("apple"),
		vkey("key", "apple"):  "value",
		vkey("key2", "apple"): "value2",
	}
	assert.Equal(t, expected, f.Dump())
}
