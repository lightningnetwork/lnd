package models

import "github.com/btcsuite/btcd/btcec/v2/ecdsa"

// ChannelAuthProof is the authentication proof (the signature portion) for a
// channel. Using the four signatures contained in the struct, and some
// auxiliary knowledge (the funding script, node identities, and outpoint) nodes
// on the network are able to validate the authenticity and existence of a
// channel. Each of these signatures signs the following digest: chanID ||
// nodeID1 || nodeID2 || bitcoinKey1|| bitcoinKey2 || 2-byte-feature-len ||
// features.
type ChannelAuthProof struct {
	// nodeSig1 is a cached instance of the first node signature.
	nodeSig1 *ecdsa.Signature

	// NodeSig1Bytes are the raw bytes of the first node signature encoded
	// in DER format.
	NodeSig1Bytes []byte

	// nodeSig2 is a cached instance of the second node signature.
	nodeSig2 *ecdsa.Signature

	// NodeSig2Bytes are the raw bytes of the second node signature
	// encoded in DER format.
	NodeSig2Bytes []byte

	// bitcoinSig1 is a cached instance of the first bitcoin signature.
	bitcoinSig1 *ecdsa.Signature

	// BitcoinSig1Bytes are the raw bytes of the first bitcoin signature
	// encoded in DER format.
	BitcoinSig1Bytes []byte

	// bitcoinSig2 is a cached instance of the second bitcoin signature.
	bitcoinSig2 *ecdsa.Signature

	// BitcoinSig2Bytes are the raw bytes of the second bitcoin signature
	// encoded in DER format.
	BitcoinSig2Bytes []byte
}

// Node1Sig is the signature using the identity key of the node that is first
// in a lexicographical ordering of the serialized public keys of the two nodes
// that created the channel.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelAuthProof) Node1Sig() (*ecdsa.Signature, error) {
	if c.nodeSig1 != nil {
		return c.nodeSig1, nil
	}

	sig, err := ecdsa.ParseSignature(c.NodeSig1Bytes)
	if err != nil {
		return nil, err
	}

	c.nodeSig1 = sig

	return sig, nil
}

// Node2Sig is the signature using the identity key of the node that is second
// in a lexicographical ordering of the serialized public keys of the two nodes
// that created the channel.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelAuthProof) Node2Sig() (*ecdsa.Signature, error) {
	if c.nodeSig2 != nil {
		return c.nodeSig2, nil
	}

	sig, err := ecdsa.ParseSignature(c.NodeSig2Bytes)
	if err != nil {
		return nil, err
	}

	c.nodeSig2 = sig

	return sig, nil
}

// BitcoinSig1 is the signature using the public key of the first node that was
// used in the channel's multi-sig output.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelAuthProof) BitcoinSig1() (*ecdsa.Signature, error) {
	if c.bitcoinSig1 != nil {
		return c.bitcoinSig1, nil
	}

	sig, err := ecdsa.ParseSignature(c.BitcoinSig1Bytes)
	if err != nil {
		return nil, err
	}

	c.bitcoinSig1 = sig

	return sig, nil
}

// BitcoinSig2 is the signature using the public key of the second node that
// was used in the channel's multi-sig output.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelAuthProof) BitcoinSig2() (*ecdsa.Signature, error) {
	if c.bitcoinSig2 != nil {
		return c.bitcoinSig2, nil
	}

	sig, err := ecdsa.ParseSignature(c.BitcoinSig2Bytes)
	if err != nil {
		return nil, err
	}

	c.bitcoinSig2 = sig

	return sig, nil
}

// IsEmpty check is the authentication proof is empty Proof is empty if at
// least one of the signatures are equal to nil.
func (c *ChannelAuthProof) IsEmpty() bool {
	return len(c.NodeSig1Bytes) == 0 ||
		len(c.NodeSig2Bytes) == 0 ||
		len(c.BitcoinSig1Bytes) == 0 ||
		len(c.BitcoinSig2Bytes) == 0
}
