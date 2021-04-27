// +build kvdb_etcd

package etcd

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

func TestTxManualCommit(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	require.NoError(t, err)

	tx, err := db.BeginReadWriteTx()
	require.NoError(t, err)
	require.NotNil(t, tx)

	committed := false

	tx.OnCommit(func() {
		committed = true
	})

	apple, err := tx.CreateTopLevelBucket([]byte("apple"))
	require.NoError(t, err)
	require.NotNil(t, apple)
	require.NoError(t, apple.Put([]byte("testKey"), []byte("testVal")))

	banana, err := tx.CreateTopLevelBucket([]byte("banana"))
	require.NoError(t, err)
	require.NotNil(t, banana)
	require.NoError(t, banana.Put([]byte("testKey"), []byte("testVal")))
	require.NoError(t, tx.DeleteTopLevelBucket([]byte("banana")))

	require.NoError(t, tx.Commit())
	require.True(t, committed)

	expected := map[string]string{
		bkey("apple"):            bval("apple"),
		vkey("testKey", "apple"): "testVal",
	}
	require.Equal(t, expected, f.Dump())
}

func TestTxRollback(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	require.NoError(t, err)

	tx, err := db.BeginReadWriteTx()
	require.Nil(t, err)
	require.NotNil(t, tx)

	apple, err := tx.CreateTopLevelBucket([]byte("apple"))
	require.Nil(t, err)
	require.NotNil(t, apple)

	require.NoError(t, apple.Put([]byte("testKey"), []byte("testVal")))

	require.NoError(t, tx.Rollback())
	require.Error(t, walletdb.ErrTxClosed, tx.Commit())
	require.Equal(t, map[string]string{}, f.Dump())
}

func TestChangeDuringManualTx(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	require.NoError(t, err)

	tx, err := db.BeginReadWriteTx()
	require.Nil(t, err)
	require.NotNil(t, tx)

	apple, err := tx.CreateTopLevelBucket([]byte("apple"))
	require.Nil(t, err)
	require.NotNil(t, apple)

	require.NoError(t, apple.Put([]byte("testKey"), []byte("testVal")))

	// Try overwriting the bucket key.
	f.Put(bkey("apple"), "banana")

	// TODO: translate error
	require.NotNil(t, tx.Commit())
	require.Equal(t, map[string]string{
		bkey("apple"): "banana",
	}, f.Dump())
}

func TestChangeDuringUpdate(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	require.NoError(t, err)

	count := 0

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		require.NoError(t, err)
		require.NotNil(t, apple)

		require.NoError(t, apple.Put([]byte("key"), []byte("value")))

		if count == 0 {
			f.Put(vkey("key", "apple"), "new_value")
			f.Put(vkey("key2", "apple"), "value2")
		}

		cursor := apple.ReadCursor()
		k, v := cursor.First()
		require.Equal(t, []byte("key"), k)
		require.Equal(t, []byte("value"), v)
		require.Equal(t, v, apple.Get([]byte("key")))

		k, v = cursor.Next()
		if count == 0 {
			require.Nil(t, k)
			require.Nil(t, v)
		} else {
			require.Equal(t, []byte("key2"), k)
			require.Equal(t, []byte("value2"), v)
		}

		count++
		return nil
	})

	require.Nil(t, err)
	require.Equal(t, count, 2)

	expected := map[string]string{
		bkey("apple"):         bval("apple"),
		vkey("key", "apple"):  "value",
		vkey("key2", "apple"): "value2",
	}
	require.Equal(t, expected, f.Dump())
}
