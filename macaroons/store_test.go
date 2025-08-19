package macaroons_test

import (
	"context"
	"crypto/rand"
	"path"
	"testing"

	"github.com/btcsuite/btcwallet/snacl"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
)

var (
	defaultRootKeyIDContext = macaroons.ContextWithRootKeyID(
		context.Background(), macaroons.DefaultRootKeyID,
	)

	nonDefaultRootKeyIDContext = macaroons.ContextWithRootKeyID(
		context.Background(), []byte{1},
	)
)

// newTestStore creates a new bolt DB in a temporary directory and then
// initializes a root key storage for that DB.
func newTestStore(t *testing.T) (string, *macaroons.RootKeyStorage) {
	tempDir := t.TempDir()

	store := openTestStore(t, tempDir)

	return tempDir, store
}

// openTestStore opens an existing bolt DB and then initializes a root key
// storage for that DB.
func openTestStore(t *testing.T, tempDir string) *macaroons.RootKeyStorage {
	db, err := kvdb.Create(
		kvdb.BoltBackendName, path.Join(tempDir, "weks.db"), true,
		kvdb.DefaultDBTimeout, false,
	)
	require.NoError(t, err)

	store, err := macaroons.NewRootKeyStorage(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("Error creating root key store: %v", err)
	}

	t.Cleanup(func() {
		_ = store.Close()
		_ = db.Close()
	})

	return store
}

// TestStore tests the normal use cases of the store like creating, unlocking,
// reading keys and closing it.
func TestStore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tempDir, store := newTestStore(t)

	_, _, err := store.RootKey(ctx)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	_, err = store.Get(ctx, nil)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	require.NoError(t, err)

	// Check ErrContextRootKeyID is returned when no root key ID found in
	// context.
	_, _, err = store.RootKey(ctx)
	require.Equal(t, macaroons.ErrContextRootKeyID, err)

	// Check ErrMissingRootKeyID is returned when empty root key ID is used.
	emptyKeyID := make([]byte, 0)
	badCtx := macaroons.ContextWithRootKeyID(ctx, emptyKeyID)
	_, _, err = store.RootKey(badCtx)
	require.Equal(t, macaroons.ErrMissingRootKeyID, err)

	// Create a context with illegal root key ID value.
	encryptedKeyID := []byte("enckey")
	badCtx = macaroons.ContextWithRootKeyID(ctx, encryptedKeyID)
	_, _, err = store.RootKey(badCtx)
	require.Equal(t, macaroons.ErrKeyValueForbidden, err)

	// Create a context with root key ID value.
	key, id, err := store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)

	rootID := id
	require.Equal(t, macaroons.DefaultRootKeyID, rootID)

	key2, err := store.Get(defaultRootKeyIDContext, id)
	require.NoError(t, err)
	require.Equal(t, key, key2)

	badpw := []byte("badweks")
	err = store.CreateUnlock(&badpw)
	require.Equal(t, macaroons.ErrAlreadyUnlocked, err)

	_ = store.Close()
	_ = store.Backend.Close()

	// Between here and the re-opening of the store, it's possible to get
	// a double-close, but that's not such a big deal since the tests will
	// fail anyway in that case.
	store = openTestStore(t, tempDir)

	err = store.CreateUnlock(&badpw)
	require.Equal(t, snacl.ErrInvalidPassword, err)

	err = store.CreateUnlock(nil)
	require.Equal(t, macaroons.ErrPasswordRequired, err)

	_, _, err = store.RootKey(defaultRootKeyIDContext)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	_, err = store.Get(defaultRootKeyIDContext, nil)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	err = store.CreateUnlock(&pw)
	require.NoError(t, err)

	key, err = store.Get(defaultRootKeyIDContext, rootID)
	require.NoError(t, err)
	require.Equal(t, key, key2)

	key, id, err = store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)
	require.Equal(t, key, key2)
	require.Equal(t, rootID, id)
}

// TestStoreGenerateNewRootKey tests that root keys can be replaced with new
// ones in the store without changing the password.
func TestStoreGenerateNewRootKey(t *testing.T) {
	_, store := newTestStore(t)

	// The store must be unlocked to replace the root key.
	err := store.GenerateNewRootKey()
	require.Equal(t, macaroons.ErrStoreLocked, err)

	// Unlock the store.
	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	require.NoError(t, err)

	// Read the default root key.
	oldRootKey1, _, err := store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)

	// Read the non-default root-key.
	oldRootKey2, _, err := store.RootKey(nonDefaultRootKeyIDContext)
	require.NoError(t, err)

	// Replace the root keys with new random keys.
	err = store.GenerateNewRootKey()
	require.NoError(t, err)

	// Finally, read both root keys from the DB and compare them to the ones
	// we got returned earlier. This makes sure that the encryption/
	// decryption of the key in the DB worked as expected too.
	newRootKey1, _, err := store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)
	require.NotEqual(t, oldRootKey1, newRootKey1)

	newRootKey2, _, err := store.RootKey(nonDefaultRootKeyIDContext)
	require.NoError(t, err)
	require.NotEqual(t, oldRootKey2, newRootKey2)
}

// TestStoreSetRootKey tests that a root key can be set to a specified value.
func TestStoreSetRootKey(t *testing.T) {
	_, store := newTestStore(t)

	// Create a new random key
	rootKey := make([]byte, 32)
	_, err := rand.Read(rootKey)
	require.NoError(t, err)

	// The store must be unlocked to set the root key.
	err = store.SetRootKey(rootKey)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	// Unlock the store and read the current key.
	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	require.NoError(t, err)
	oldRootKey, _, err := store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)

	// Ensure the new key is different from the old key.
	require.NotEqual(t, oldRootKey, rootKey)

	// Replace the root key with the new key.
	err = store.SetRootKey(rootKey)
	require.NoError(t, err)

	// Finally, read the root key from the DB and compare it to the one
	// we created earlier. This makes sure that the encryption/
	// decryption of the key in the DB worked as expected too.
	newRootKey, _, err := store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)
	require.Equal(t, rootKey, newRootKey)
}

// TestStoreChangePassword tests that the password for the store can be changed
// without changing the root keys.
func TestStoreChangePassword(t *testing.T) {
	tempDir, store := newTestStore(t)

	// The store must be unlocked to replace the root keys.
	err := store.ChangePassword(nil, nil)
	require.Equal(t, macaroons.ErrStoreLocked, err)

	// Unlock the DB and read the current default root key and one other
	// non-default root key. Both of these should stay the same after
	// changing the password for the test to succeed.
	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	require.NoError(t, err)

	rootKey1, _, err := store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)

	rootKey2, _, err := store.RootKey(nonDefaultRootKeyIDContext)
	require.NoError(t, err)

	// Both passwords must be set.
	err = store.ChangePassword(nil, nil)
	require.Equal(t, macaroons.ErrPasswordRequired, err)

	// Make sure that an error is returned if we try to change the password
	// without the correct old password.
	wrongPw := []byte("wrong")
	newPw := []byte("newpassword")
	err = store.ChangePassword(wrongPw, newPw)
	require.Equal(t, snacl.ErrInvalidPassword, err)

	// Now really do change the password.
	err = store.ChangePassword(pw, newPw)
	require.NoError(t, err)

	// Close the store. This will close the underlying DB and we need to
	// create a new store instance. Let's make sure we can't use it again
	// after closing.
	err = store.Close()
	require.NoError(t, err)
	err = store.Backend.Close()
	require.NoError(t, err)

	err = store.CreateUnlock(&newPw)
	require.Error(t, err)

	// Let's open it again and try unlocking with the new password.
	store = openTestStore(t, tempDir)
	err = store.CreateUnlock(&newPw)
	require.NoError(t, err)

	// Finally, read the root keys from the DB using the new password and
	// make sure that both root keys stayed the same.
	rootKeyDB1, _, err := store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)
	require.Equal(t, rootKey1, rootKeyDB1)

	rootKeyDB2, _, err := store.RootKey(nonDefaultRootKeyIDContext)
	require.NoError(t, err)
	require.Equal(t, rootKey2, rootKeyDB2)
}
