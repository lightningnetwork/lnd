package bolt12handler

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
)

// NodeSigner provides the node's identity key for BOLT 12 invoice signing.
type NodeSigner interface {
	// NodePubKey returns the node's identity public key.
	NodePubKey() *btcec.PublicKey

	// SignInvoice signs a BOLT 12 invoice with a BIP-340 Schnorr signature
	// from the node's identity private key.
	SignInvoice(inv *bolt12.Invoice) ([64]byte, error)
}
