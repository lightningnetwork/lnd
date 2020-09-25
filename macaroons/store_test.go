package macaroons_test

import (
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/macaroons"

	"github.com/btcsuite/btcwallet/snacl"
)

// newTestStore creates a new bolt DB in a temporary directory and then
// initializes a root key storage for that DB.
func newTestStore(t *testing.T) (string, func(),
	*macaroons.RootKeyStorage) {
	tempDir, err := ioutil.TempDir("", "macaroonstore-")
	if err != nil {
		t.Fatalf("Error creating temp dir: %v", err)
	}

	cleanup, store := openTestStore(t, tempDir)
	cleanup2 := func() {
		cleanup()
		os.RemoveAll(tempDir)
	}

	return tempDir, cleanup2, store
}

// openTestStore opens an existing bolt DB and then initializes a root key
// storage for that DB.
func openTestStore(t *testing.T, tempDir string) (func(),
	*macaroons.RootKeyStorage) {

	db, err := kvdb.Create(
		kvdb.BoltBackendName, path.Join(tempDir, "weks.db"), true,
	)
	if err != nil {
		t.Fatalf("Error opening store DB: %v", err)
	}

	store, err := macaroons.NewRootKeyStorage(db)
	if err != nil {
		db.Close()
		t.Fatalf("Error creating root key store: %v", err)
	}

	cleanup := func() {
		store.Close()
	}

	return cleanup, store
}

// TestStore tests the normal use cases of the store like creating, unlocking,
// reading keys and closing it.
func TestStore(t *testing.T) {
	tempDir, cleanup, store := newTestStore(t)
	defer cleanup()

	_, _, err := store.RootKey(context.TODO())
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	_, err = store.Get(context.TODO(), nil)
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	if err != nil {
		t.Fatalf("Error creating store encryption key: %v", err)
	}

	key, id, err := store.RootKey(context.TODO())
	if err != nil {
		t.Fatalf("Error getting root key from store: %v", err)
	}
	rootID := id

	key2, err := store.Get(context.TODO(), id)
	if err != nil {
		t.Fatalf("Error getting key with ID %s: %v", string(id), err)
	}
	if !bytes.Equal(key, key2) {
		t.Fatalf("Root key doesn't match: expected %v, got %v",
			key, key2)
	}

	badpw := []byte("badweks")
	err = store.CreateUnlock(&badpw)
	if err != macaroons.ErrAlreadyUnlocked {
		t.Fatalf("Received %v instead of ErrAlreadyUnlocked", err)
	}

	store.Close()

	// Between here and the re-opening of the store, it's possible to get
	// a double-close, but that's not such a big deal since the tests will
	// fail anyway in that case.
	_, store = openTestStore(t, tempDir)

	err = store.CreateUnlock(&badpw)
	if err != snacl.ErrInvalidPassword {
		t.Fatalf("Received %v instead of ErrInvalidPassword", err)
	}

	err = store.CreateUnlock(nil)
	if err != macaroons.ErrPasswordRequired {
		t.Fatalf("Received %v instead of ErrPasswordRequired", err)
	}

	_, _, err = store.RootKey(context.TODO())
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	_, err = store.Get(context.TODO(), nil)
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	err = store.CreateUnlock(&pw)
	if err != nil {
		t.Fatalf("Error unlocking root key store: %v", err)
	}

	key, err = store.Get(context.TODO(), rootID)
	if err != nil {
		t.Fatalf("Error getting key with ID %s: %v",
			string(rootID), err)
	}
	if !bytes.Equal(key, key2) {
		t.Fatalf("Root key doesn't match: expected %v, got %v",
			key2, key)
	}

	key, id, err = store.RootKey(context.TODO())
	if err != nil {
		t.Fatalf("Error getting root key from store: %v", err)
	}
	if !bytes.Equal(key, key2) {
		t.Fatalf("Root key doesn't match: expected %v, got %v",
			key2, key)
	}
	if !bytes.Equal(rootID, id) {
		t.Fatalf("Root ID doesn't match: expected %v, got %v",
			rootID, id)
	}
}

// TestStoreGenerateNewRootKey tests that a root key can be replaced with a new
// one in the store without changing the password.
func TestStoreGenerateNewRootKey(t *testing.T) {
	_, cleanup, store := newTestStore(t)
	defer cleanup()

	// The store must be unlocked to replace the root key.
	err := store.GenerateNewRootKey()
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	// Unlock the store and read the current key.
	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	if err != nil {
		t.Fatalf("Error creating store encryption key: %v", err)
	}
	oldRootKey, _, err := store.RootKey(context.Background())
	if err != nil {
		t.Fatalf("Error getting root key from store: %v", err)
	}

	// Replace the root key with a new random key.
	err = store.GenerateNewRootKey()
	if err != nil {
		t.Fatalf("Error replacing root key: %v", err)
	}

	// Finally, read the root key from the DB and compare it to the one
	// we got returned earlier. This makes sure that the encryption/
	// decryption of the key in the DB worked as expected too.
	newRootKey, _, err := store.RootKey(context.Background())
	if err != nil {
		t.Fatalf("Error getting root key from store: %v", err)
	}
	if bytes.Equal(oldRootKey, newRootKey) {
		t.Fatalf("New root key is equal to old one: %v", newRootKey)
	}
}

// TestStoreChangePassword tests that the password for the store can be changed
// without changing the root key.
func TestStoreChangePassword(t *testing.T) {
	tempDir, cleanup, store := newTestStore(t)
	defer cleanup()

	// The store must be unlocked to replace the root key.
	err := store.ChangePassword(nil, nil)
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	// Unlock the DB and read the current root key. This will need to stay
	// the same after changing the password for the test to succeed.
	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	if err != nil {
		t.Fatalf("Error creating store encryption key: %v", err)
	}
	rootKey, _, err := store.RootKey(context.Background())
	if err != nil {
		t.Fatalf("Error getting root key from store: %v", err)
	}

	// Both passwords must be set.
	err = store.ChangePassword(nil, nil)
	if err != macaroons.ErrPasswordRequired {
		t.Fatalf("Received %v instead of ErrPasswordRequired", err)
	}

	// Make sure that an error is returned if we try to change the password
	// without the correct old password.
	wrongPw := []byte("wrong")
	newPw := []byte("newpassword")
	err = store.ChangePassword(wrongPw, newPw)
	if err == nil {
		t.Fatalf("Did not receive error when using wrong password")
	}

	// Now really do change the password.
	err = store.ChangePassword(pw, newPw)
	if err != nil {
		t.Fatalf("Error changing the password: %v", err)
	}

	// Close the store and unlock it with the new password.
	store.Close()
	_, store = openTestStore(t, tempDir)
	err = store.CreateUnlock(&newPw)
	if err != nil {
		t.Fatalf("Error creating store encryption key: %v", err)
	}

	// Finally read the root key from the DB using the new password and
	// make sure the root key stayed the same.
	rootKeyDb, _, err := store.RootKey(context.Background())
	if err != nil {
		t.Fatalf("Error getting root key from store: %v", err)
	}
	if !bytes.Equal(rootKey, rootKeyDb) {
		t.Fatalf("Root key doesn't match: expected %v, got %v",
			rootKey, rootKeyDb)
	}
}
