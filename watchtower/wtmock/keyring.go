package wtmock

import (
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/keychain"
)

// SecretKeyRing is a mock, in-memory implementation for deriving private keys.
type SecretKeyRing struct {
	mu   sync.Mutex
	keys map[keychain.KeyLocator]*btcec.PrivateKey
}

// NewSecretKeyRing creates a new mock SecretKeyRing.
func NewSecretKeyRing() *SecretKeyRing {
	return &SecretKeyRing{
		keys: make(map[keychain.KeyLocator]*btcec.PrivateKey),
	}
}

// DerivePrivKey derives the private key for a given key descriptor. If
// this method is called twice with the same argument, it will return the same
// private key.
func (m *SecretKeyRing) DerivePrivKey(
	desc keychain.KeyDescriptor) (*btcec.PrivateKey, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if key, ok := m.keys[desc.KeyLocator]; ok {
		return key, nil
	}

	privKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	m.keys[desc.KeyLocator] = privKey

	return privKey, nil
}
