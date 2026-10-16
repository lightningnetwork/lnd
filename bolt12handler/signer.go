package bolt12handler

import (
	"github.com/btcsuite/btcd/btcec/v2"
)

// NodeSigner provides the node's identity key for BOLT 12 invoice signing.
type NodeSigner interface {
	// NodePubKey returns the node's identity public key.
	NodePubKey() *btcec.PublicKey

	// SignInvoiceRoot returns a BIP-340 Schnorr signature by the node's
	// identity key over the tagged hash of root, with the tag
	// bolt12.InvoiceSignatureTag. The root comes from
	// bolt12.InvoiceSigningRoot.
	SignInvoiceRoot(root [32]byte) ([64]byte, error)
}
