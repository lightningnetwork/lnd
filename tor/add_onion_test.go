package tor

import (
	"bytes"
	"io/ioutil"
	"path/filepath"
	"testing"
)

// TestOnionFile tests that the OnionFile implementation of the OnionStore
// interface behaves as expected.
func TestOnionFile(t *testing.T) {
	t.Parallel()

	tempDir, err := ioutil.TempDir("", "onion_store")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}

	privateKey := []byte("hide_me_plz")
	privateKeyPath := filepath.Join(tempDir, "secret")

	// Create a new file-based onion store. A private key should not exist
	// yet.
	onionFile := NewOnionFile(privateKeyPath, 0600)
	if _, err := onionFile.PrivateKey(V2); err != ErrNoPrivateKey {
		t.Fatalf("expected ErrNoPrivateKey, got \"%v\"", err)
	}

	// Store the private key and ensure what's stored matches.
	if err := onionFile.StorePrivateKey(V2, privateKey); err != nil {
		t.Fatalf("unable to store private key: %v", err)
	}
	storePrivateKey, err := onionFile.PrivateKey(V2)
	if err != nil {
		t.Fatalf("unable to retrieve private key: %v", err)
	}
	if !bytes.Equal(storePrivateKey, privateKey) {
		t.Fatalf("expected private key \"%v\", got \"%v\"",
			string(privateKey), string(storePrivateKey))
	}

	// Finally, delete the private key. We should no longer be able to
	// retrieve it.
	if err := onionFile.DeletePrivateKey(V2); err != nil {
		t.Fatalf("unable to delete private key: %v", err)
	}
	if _, err := onionFile.PrivateKey(V2); err != ErrNoPrivateKey {
		t.Fatal("found deleted private key")
	}
}
