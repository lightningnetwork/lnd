package macaroons_test

import (
	"bytes"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/coreos/bbolt"

	"github.com/lightningnetwork/lnd/macaroons"

	"github.com/btcsuite/btcwallet/snacl"
)

func TestStore(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "macaroonstore-")
	if err != nil {
		t.Fatalf("Error creating temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	db, err := bolt.Open(path.Join(tempDir, "weks.db"), 0600,
		bolt.DefaultOptions)
	if err != nil {
		t.Fatalf("Error opening store DB: %v", err)
	}

	store, err := macaroons.NewRootKeyStorage(db)
	if err != nil {
		db.Close()
		t.Fatalf("Error creating root key store: %v", err)
	}
	defer store.Close()

	key, id, err := store.RootKey(nil)
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	key, err = store.Get(nil, nil)
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	pw := []byte("weks")
	err = store.CreateUnlock(&pw)
	if err != nil {
		t.Fatalf("Error creating store encryption key: %v", err)
	}

	key, id, err = store.RootKey(nil)
	if err != nil {
		t.Fatalf("Error getting root key from store: %v", err)
	}
	rootID := id

	key2, err := store.Get(nil, id)
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
	db, err = bolt.Open(path.Join(tempDir, "weks.db"), 0600,
		bolt.DefaultOptions)
	if err != nil {
		t.Fatalf("Error opening store DB: %v", err)
	}

	store, err = macaroons.NewRootKeyStorage(db)
	if err != nil {
		db.Close()
		t.Fatalf("Error creating root key store: %v", err)
	}

	err = store.CreateUnlock(&badpw)
	if err != snacl.ErrInvalidPassword {
		t.Fatalf("Received %v instead of ErrInvalidPassword", err)
	}

	err = store.CreateUnlock(nil)
	if err != macaroons.ErrPasswordRequired {
		t.Fatalf("Received %v instead of ErrPasswordRequired", err)
	}

	key, id, err = store.RootKey(nil)
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	key, err = store.Get(nil, nil)
	if err != macaroons.ErrStoreLocked {
		t.Fatalf("Received %v instead of ErrStoreLocked", err)
	}

	err = store.CreateUnlock(&pw)
	if err != nil {
		t.Fatalf("Error unlocking root key store: %v", err)
	}

	key, err = store.Get(nil, rootID)
	if err != nil {
		t.Fatalf("Error getting key with ID %s: %v",
			string(rootID), err)
	}
	if !bytes.Equal(key, key2) {
		t.Fatalf("Root key doesn't match: expected %v, got %v",
			key2, key)
	}

	key, id, err = store.RootKey(nil)
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
