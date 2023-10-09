package netann

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// NodeSigner is an implementation of the MessageSigner interface backed by the
// identity private key of running lnd node.
type NodeSigner struct {
	keySigner keychain.SingleKeyMessageSigner
}

// NewNodeSigner creates a new instance of the NodeSigner backed by the target
// private key.
func NewNodeSigner(keySigner keychain.SingleKeyMessageSigner) *NodeSigner {
	return &NodeSigner{
		keySigner: keySigner,
	}
}

// SignMessage signs a double-sha256 digest of the passed msg under the
// resident node's private key described in the key locator. If the target key
// locator is _not_ the node's private key, then an error will be returned.
func (n *NodeSigner) SignMessage(keyLoc keychain.KeyLocator,
	msg []byte, doubleHash bool) (*ecdsa.Signature, error) {

	// If this isn't our identity public key, then we'll exit early with an
	// error as we can't sign with this key.
	if keyLoc != n.keySigner.KeyLocator() {
		return nil, fmt.Errorf("unknown public key locator")
	}

	// Otherwise, we'll sign the double-sha256 of the target message.
	sig, err := n.keySigner.SignMessage(msg, doubleHash)
	if err != nil {
		return nil, fmt.Errorf("can't sign the message: %v", err)
	}

	return sig, nil
}

// SignMuSig2 generates a MuSig2 partial signature given the passed key set,
// secret nonce, public nonce, and private keys.
func (n *NodeSigner) SignMuSig2(secNonce [musig2.SecNonceSize]byte,
	keyLoc keychain.KeyLocator, otherNonces [][musig2.PubNonceSize]byte,
	combinedNonce [musig2.PubNonceSize]byte, pubKeys []*btcec.PublicKey,
	msg [32]byte, opts ...musig2.SignOption) (*musig2.PartialSignature,
	error) {

	// If this isn't our identity public key, then we'll exit early with an
	// error as we can't sign with this key.
	if keyLoc != n.keySigner.KeyLocator() {
		return nil, fmt.Errorf("unknown public key locator")
	}

	return n.keySigner.SignMuSig2(
		secNonce, keyLoc, otherNonces, combinedNonce, pubKeys, msg,
		opts...,
	)
}

// SignMessageCompact signs a single or double sha256 digest of the msg
// parameter under the resident node's private key. The returned signature is a
// pubkey-recoverable signature.
func (n *NodeSigner) SignMessageCompact(msg []byte, doubleHash bool) ([]byte,
	error) {

	return n.keySigner.SignMessageCompact(msg, doubleHash)
}

// A compile time check to ensure that NodeSigner implements the MessageSigner
// interface.
var _ lnwallet.MessageSigner = (*NodeSigner)(nil)
