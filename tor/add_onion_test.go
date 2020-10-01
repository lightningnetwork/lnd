package tor

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	testWalletPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}
)

type mockKeyRing struct {
	fail bool
}

func (m *mockKeyRing) DeriveNextKey(keyFam keychain.KeyFamily) (keychain.KeyDescriptor, error) {
	return keychain.KeyDescriptor{}, nil
}

func (m *mockKeyRing) DeriveKey(keyLoc keychain.KeyLocator) (keychain.KeyDescriptor, error) {
	if m.fail {
		return keychain.KeyDescriptor{}, fmt.Errorf("fail")
	}

	_, pub := btcec.PrivKeyFromBytes(btcec.S256(), testWalletPrivKey)
	return keychain.KeyDescriptor{
		PubKey: pub,
	}, nil
}

// testOnionFile tests that the OnionFile implementation of the OnionStore
// interface behaves as expected.
func testOnionFile(withEncryption bool) error {

	// create a mock KeyRing to test encryption
	keyRing := &mockKeyRing{}

	tempDir, err := ioutil.TempDir("", "onion_store")
	if err != nil {
		return fmt.Errorf("unable to create temp dir: %v", err)
	}

	privateKey := []byte("RSA1024:hide_me_plz")
	privateKeyPath := filepath.Join(tempDir, "secret")

	// Create a new file-based onion store. A private key should not exist
	// yet.
	onionFile := NewOnionFile(privateKeyPath, 0600, withEncryption, keyRing)
	if _, err := onionFile.PrivateKey(V2); err != ErrNoPrivateKey {
		return fmt.Errorf("expected ErrNoPrivateKey, got \"%v\"", err)
	}

	// Store the private key and ensure what's stored matches.
	if err := onionFile.StorePrivateKey(V2, privateKey); err != nil {
		return fmt.Errorf("unable to store private key: %v", err)
	}
	storePrivateKey, err := onionFile.PrivateKey(V2)
	if err != nil {
		return fmt.Errorf("unable to retrieve private key: %v", err)
	}
	if !bytes.Equal(storePrivateKey, privateKey) {
		return fmt.Errorf("expected private key \"%v\", got \"%v\"",
			string(privateKey), string(storePrivateKey))
	}

	// Finally, delete the private key. We should no longer be able to
	// retrieve it.
	if err := onionFile.DeletePrivateKey(V2); err != nil {
		return fmt.Errorf("unable to delete private key: %v", err)
	}
	if _, err := onionFile.PrivateKey(V2); err != ErrNoPrivateKey {
		return fmt.Errorf("found deleted private key")
	}
	return nil
}

// TestOnionFileWithoutEncryption tests that the OnionFile implementation
// of the OnionStore interface behaves as expected with encryption disabled..
func TestOnionFileWithoutEncryption(t *testing.T) {
	t.Parallel()
	err := testOnionFile(false)
	if err != nil {
		t.Fatal(err)
	}
}

// TestEncryptedOnionFile tests that the OnionFile implementation
// of the OnionStore interface behaves as expected with encryption enabled..
func TestOnionFileWithEncryption(t *testing.T) {
	t.Parallel()
	err := testOnionFile(true)
	if err != nil {
		t.Fatal(err)
	}
}
