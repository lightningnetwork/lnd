package bolt12handler

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/keychain"
)

// KeyRingSigner implements NodeSigner for the daemon. It signs through the
// Schnorr message signer of the key ring and never sees the private key, so it
// also works in remote signing mode.
type KeyRingSigner struct {
	signer      keychain.MessageSignerRing
	keyLoc      keychain.KeyLocator
	identityPub *btcec.PublicKey
}

// NewKeyRingSigner creates a NodeSigner backed by the daemon's key ring.
func NewKeyRingSigner(signer keychain.MessageSignerRing,
	keyLoc keychain.KeyLocator,
	identityPub *btcec.PublicKey) *KeyRingSigner {

	return &KeyRingSigner{
		signer:      signer,
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

// SignInvoiceRoot signs an invoice Merkle root with the node's identity key.
//
// NOTE: This is part of the NodeSigner interface.
func (s *KeyRingSigner) SignInvoiceRoot(root [32]byte) ([64]byte, error) {
	sig, err := s.signer.SignMessageSchnorr(
		s.keyLoc, root[:], false, nil,
		[]byte(bolt12.InvoiceSignatureTag),
	)
	if err != nil {
		return [64]byte{}, fmt.Errorf("sign invoice root: %w", err)
	}

	return [64]byte(sig.Serialize()), nil
}
