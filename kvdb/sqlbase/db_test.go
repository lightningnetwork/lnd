//go:build !js

package sqlbase

import (
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestExternalDriver preserves bucket semantics and shared connection ownership
// when an application supplies its SQL driver without the kvdb_sqlite tag.
func TestExternalDriver(t *testing.T) {
	Init(1)
	cfg := Config{
		DriverName:      "sqlite",
		Dsn:             filepath.Join(t.TempDir(), "channels.sqlite"),
		TableNamePrefix: "channels",
		WithTxLevelLock: true,
	}
	store, err := NewSqlBackend(t.Context(), &cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	invalid := cfg
	invalid.TableNamePrefix = "invalid-prefix"
	_, err = NewSqlBackend(t.Context(), &invalid)
	require.Error(t, err)

	require.NoError(t, store.Update(func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket([]byte("state"))
		if err != nil {
			return err
		}
		_, err = bucket.CreateBucket([]byte("nested"))
		if err != nil {
			return err
		}

		return bucket.Put([]byte("empty"), []byte{})
	}, func() {}))
	require.NoError(t, store.View(func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte("state"))
		require.Nil(t, bucket.Get([]byte("nested")))
		require.NotNil(t, bucket.NestedReadBucket([]byte("nested")))
		require.NotNil(t, bucket.Get([]byte("empty")))
		require.Nil(t, bucket.Get([]byte("missing")))
		check := func(key, value []byte) error {
			if string(key) == "empty" {
				require.NotNil(t, value)
				require.Empty(t, value)
			} else {
				require.Nil(t, value)
			}

			return nil
		}
		require.NoError(t, bucket.ForEach(check))
		cursor := bucket.ReadCursor()
		key, value := cursor.First()
		require.NoError(t, check(key, value))
		key, value = cursor.Last()
		require.NoError(t, check(key, value))
		key, value = cursor.Prev()
		require.NoError(t, check(key, value))
		key, value = cursor.Seek([]byte("empty"))
		require.NoError(t, check(key, value))
		sqlBucket, ok := bucket.(*readWriteBucket)
		require.True(t, ok)
		require.NoError(t, sqlBucket.ForAll(check))

		return nil
	}, func() {}))

	otherCfg := cfg
	otherCfg.TableNamePrefix = "other"
	other, err := NewSqlBackend(t.Context(), &otherCfg)
	require.NoError(t, err)
	require.NoError(t, other.View(func(tx walletdb.ReadTx) error {
		require.Nil(t, tx.ReadBucket([]byte("state")))
		return nil
	}, func() {}))
	require.NoError(t, other.Close())
	require.NoError(t, store.View(func(tx walletdb.ReadTx) error {
		require.NotNil(t, tx.ReadBucket([]byte("state")))
		return nil
	}, func() {}))
}
