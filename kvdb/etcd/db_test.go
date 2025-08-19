//go:build kvdb_etcd
// +build kvdb_etcd

package etcd

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

// TestDump tests that the Dump() method creates a one-to-one copy of the
// database content.
func TestDump(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)

	db, err := newEtcdBackend(t.Context(), f.BackendConfig())
	require.NoError(t, err)

	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		// "apple"
		apple, err := tx.CreateTopLevelBucket([]byte("apple"))
		require.NoError(t, err)
		require.NotNil(t, apple)

		require.NoError(t, apple.Put([]byte("key"), []byte("val")))
		return nil
	}, func() {})

	// Expect non-zero copy.
	var buf bytes.Buffer

	require.NoError(t, db.Copy(&buf))
	require.Greater(t, buf.Len(), 0)
	require.Nil(t, err)

	expected := map[string]string{
		BucketKey("apple"):       BucketVal("apple"),
		ValueKey("key", "apple"): "val",
	}
	require.Equal(t, expected, f.Dump())
}

// TestAbortContext tests that an update on the database is aborted if the
// database's main context in cancelled.
func TestAbortContext(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)

	ctx, cancel := context.WithCancel(t.Context())

	config := f.BackendConfig()

	// Pass abort context and abort right away.
	db, err := newEtcdBackend(ctx, config)
	require.NoError(t, err)
	cancel()

	// Expect that the update will fail.
	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		_, err := tx.CreateTopLevelBucket([]byte("bucket"))
		require.Error(t, err, "context canceled")

		return nil
	}, func() {})

	require.Error(t, err, "context canceled")

	// No changes in the DB.
	require.Equal(t, map[string]string{}, f.Dump())
}

// TestNewEtcdClient tests that an etcd v3 client can be created correctly.
func TestNewEtcdClient(t *testing.T) {
	t.Parallel()

	f := NewEtcdTestFixture(t)

	client, ctx, cancel, err := NewEtcdClient(
		t.Context(), f.BackendConfig(),
	)
	require.NoError(t, err)
	t.Cleanup(cancel)

	_, err = client.Put(ctx, "foo/bar", "baz")
	require.NoError(t, err)

	resp, err := client.Get(ctx, "foo/bar")
	require.NoError(t, err)

	require.Len(t, resp.Kvs, 1)
	require.Equal(t, "baz", string(resp.Kvs[0].Value))
}
