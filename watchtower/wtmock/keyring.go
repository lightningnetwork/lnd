package wtmock

import (
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
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

// DeriveKey attempts to derive an arbitrary key specified by the
// passed KeyLocator. This may be used in several recovery scenarios,
// or when manually rotating something like our current default node
// key.
//
// NOTE: This is part of the wtclient.ECDHKeyRing interface.
func (m *SecretKeyRing) DeriveKey(
	keyLoc keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if key, ok := m.keys[keyLoc]; ok {
		return keychain.KeyDescriptor{
			KeyLocator: keyLoc,
			PubKey:     key.PubKey(),
		}, nil
	}

	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return keychain.KeyDescriptor{}, err
	}

	m.keys[keyLoc] = privKey

	return keychain.KeyDescriptor{
		KeyLocator: keyLoc,
		PubKey:     privKey.PubKey(),
	}, nil
}

// ECDH performs a scalar multiplication (ECDH-like operation) between the
// target key descriptor and remote public key. The output returned will be the
// sha256 of the resulting shared point serialized in compressed format. If k is
// our private key, and P is the public key, we perform the following operation:
//
//	sx := k*P
//	s := sha256(sx.SerializeCompressed())
//
// NOTE: This is part of the wtclient.ECDHKeyRing interface.
func (m *SecretKeyRing) ECDH(keyDesc keychain.KeyDescriptor,
	pub *btcec.PublicKey) ([32]byte, error) {

	_, err := m.DeriveKey(keyDesc.KeyLocator)
	if err != nil {
		return [32]byte{}, err
	}

	privKey := m.keys[keyDesc.KeyLocator]
	ecdh := &keychain.PrivKeyECDH{PrivKey: privKey}
	return ecdh.ECDH(pub)
}
