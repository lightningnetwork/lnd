// +build kvdb_etcd

package etcd

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

func TestCopy(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	db, err := newEtcdBackend(f.BackendConfig())
	require.NoError(t, err)

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		// "apple"
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		require.NoError(t, err)
		require.NotNil(t, apple)

		require.NoError(t, apple.Put([]byte("key"), []byte("val")))
		return nil
	})

	// Expect non-zero copy.
	var buf bytes.Buffer

	require.NoError(t, db.Copy(&buf))
	require.Greater(t, buf.Len(), 0)
	require.Nil(t, err)

	expected := map[string]string{
		bkey("apple"):        bval("apple"),
		vkey("key", "apple"): "val",
	}
	require.Equal(t, expected, f.Dump())
}

func TestAbortContext(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)
	defer f.Cleanup()

	ctx, cancel := context.WithCancel(context.Background())

	config := f.BackendConfig()
	config.Ctx = ctx

	// Pass abort context and abort right away.
	db, err := newEtcdBackend(config)
	require.NoError(t, err)
	cancel()

	// Expect that the update will fail.
	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		_, err := tx.CreateTopLevelBucket([]byte("bucket"))
		require.NoError(t, err)

		return nil
	})

	require.Error(t, err, "context canceled")

	// No changes in the DB.
	require.Equal(t, map[string]string{}, f.Dump())
}
