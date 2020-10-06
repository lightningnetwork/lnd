package macaroons_test

import (
	"context"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/macaroons"

	"github.com/btcsuite/btcwallet/snacl"
	"github.com/stretchr/testify/require"
)

func TestStore(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "macaroonstore-")
	require.NoError(t, err)
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	db, err := kvdb.Create(
		kvdb.BoltBackendName, path.Join(tempDir, "weks.db"), true,
	)
	require.NoError(t, err)

	store, err := macaroons.NewRootKeyStorage(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("Error creating root key store: %v", err)
	}
	defer func() {
		_ = store.Close()
	}()

	_, _, err = store.RootKey(context.TODO())
	require.Equal(t, macaroons.ErrStoreLocked, err)

	_, err = store.Get(context.TODO(), nil)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	require.NoError(t, err)

	// Check ErrContextRootKeyID is returned when no root key ID found in
	// context.
	_, _, err = store.RootKey(context.TODO())
	require.Equal(t, macaroons.ErrContextRootKeyID, err)

	// Check ErrMissingRootKeyID is returned when empty root key ID is used.
	emptyKeyID := make([]byte, 0)
	badCtx := macaroons.ContextWithRootKeyID(context.TODO(), emptyKeyID)
	_, _, err = store.RootKey(badCtx)
	require.Equal(t, macaroons.ErrMissingRootKeyID, err)

	// Create a context with illegal root key ID value.
	encryptedKeyID := []byte("enckey")
	badCtx = macaroons.ContextWithRootKeyID(context.TODO(), encryptedKeyID)
	_, _, err = store.RootKey(badCtx)
	require.Equal(t, macaroons.ErrKeyValueForbidden, err)

	// Create a context with root key ID value.
	ctx := macaroons.ContextWithRootKeyID(
		context.TODO(), macaroons.DefaultRootKeyID,
	)
	key, id, err := store.RootKey(ctx)
	require.NoError(t, err)

	rootID := id
	require.Equal(t, macaroons.DefaultRootKeyID, rootID)

	key2, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, key, key2)

	badpw := []byte("badweks")
	err = store.CreateUnlock(&badpw)
	require.Equal(t, macaroons.ErrAlreadyUnlocked, err)

	_ = store.Close()

	// Between here and the re-opening of the store, it's possible to get
	// a double-close, but that's not such a big deal since the tests will
	// fail anyway in that case.
	db, err = kvdb.Create(
		kvdb.BoltBackendName, path.Join(tempDir, "weks.db"), true,
	)
	require.NoError(t, err)

	store, err = macaroons.NewRootKeyStorage(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("Error creating root key store: %v", err)
	}

	err = store.CreateUnlock(&badpw)
	require.Equal(t, snacl.ErrInvalidPassword, err)

	err = store.CreateUnlock(nil)
	require.Equal(t, macaroons.ErrPasswordRequired, err)

	_, _, err = store.RootKey(ctx)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	_, err = store.Get(ctx, nil)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	err = store.CreateUnlock(&pw)
	require.NoError(t, err)

	key, err = store.Get(ctx, rootID)
	require.NoError(t, err)
	require.Equal(t, key, key2)

	key, id, err = store.RootKey(ctx)
	require.NoError(t, err)
	require.Equal(t, key, key2)
	require.Equal(t, rootID, id)
}
