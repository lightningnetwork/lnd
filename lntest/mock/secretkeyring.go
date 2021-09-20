package mock

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/keychain"
)

// SecretKeyRing is a mock implementation of the SecretKeyRing interface.
type SecretKeyRing struct {
	RootKey *btcec.PrivateKey
}

// DeriveNextKey currently returns dummy values.
func (s *SecretKeyRing) DeriveNextKey(
	_ keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	return keychain.KeyDescriptor{
		PubKey: s.RootKey.PubKey(),
	}, nil
}

// DeriveKey currently returns dummy values.
func (s *SecretKeyRing) DeriveKey(
	_ keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	return keychain.KeyDescriptor{
		PubKey: s.RootKey.PubKey(),
	}, nil
}

// DerivePrivKey currently returns dummy values.
func (s *SecretKeyRing) DerivePrivKey(
	_ keychain.KeyDescriptor) (*btcec.PrivateKey, error) {

	return s.RootKey, nil
}

// ECDH currently returns dummy values.
func (s *SecretKeyRing) ECDH(_ keychain.KeyDescriptor,
	_ *btcec.PublicKey) ([32]byte, error) {

	return [32]byte{}, nil
}

// SignMessage signs the passed message and ignores the KeyDescriptor.
func (s *SecretKeyRing) SignMessage(_ keychain.KeyLocator,
	msg []byte) (*btcec.Signature, error) {

	digest := chainhash.DoubleHashB(msg)
	return s.RootKey.Sign(digest)
}

// SignMessageCompact signs the passed message.
func (s *SecretKeyRing) SignMessageCompact(_ keychain.KeyLocator,
	msg []byte) ([]byte, error) {

	digest := chainhash.DoubleHashB(msg)
	return btcec.SignCompact(btcec.S256(), s.RootKey, digest, true)
}
