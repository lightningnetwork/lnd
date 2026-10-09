package bolt12handler

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/keychain"
)

// KeyRingSigner implements NodeSigner for the daemon by signing invoices with
// the node identity key from the key ring. A remote signer cannot derive the
// key, so this signer does not work in remote signing mode.
type KeyRingSigner struct {
	keyRing     keychain.SecretKeyRing
	keyLoc      keychain.KeyLocator
	identityPub *btcec.PublicKey
}

// NewKeyRingSigner creates a NodeSigner backed by the daemon's key ring.
func NewKeyRingSigner(keyRing keychain.SecretKeyRing,
	keyLoc keychain.KeyLocator,
	identityPub *btcec.PublicKey) *KeyRingSigner {

	return &KeyRingSigner{
		keyRing:     keyRing,
		keyLoc:      keyLoc,
		identityPub: identityPub,
	}
}

// NodePubKey returns the node's identity public key.
//
// NOTE: This is part of the NodeSigner interface.
func (s *KeyRingSigner) NodePubKey() *btcec.PublicKey {
	return s.identityPub
}

// SignInvoice signs a BOLT 12 invoice using the node's identity private key
// derived from the key ring.
//
// NOTE: This is part of the NodeSigner interface.
func (s *KeyRingSigner) SignInvoice(inv *bolt12.Invoice) ([64]byte, error) {
	privKey, err := s.derivePrivKey()
	if err != nil {
		return [64]byte{}, err
	}

	return bolt12.SignInvoice(inv, privKey)
}

// derivePrivKey extracts the raw private key from the key ring.
func (s *KeyRingSigner) derivePrivKey() (*btcec.PrivateKey, error) {
	privKey, err := s.keyRing.DerivePrivKey(
		keychain.KeyDescriptor{
			KeyLocator: s.keyLoc,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("derive identity key: %w", err)
	}

	return privKey, nil
}
