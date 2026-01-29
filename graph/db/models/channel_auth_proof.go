package models

import (
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelAuthProof is the authentication proof (the signature portion) for a
// channel.
//
// For v1 channels:
// Using the four node and bitcoin signatures contained in the struct, and some
// auxiliary knowledge (the funding script, node identities, and outpoint) nodes
// on the network are able to validate the authenticity and existence of a
// channel. Each of these signatures signs the following digest: chanID ||
// nodeID1 || nodeID2 || bitcoinKey1|| bitcoinKey2 || 2-byte-feature-len ||
// features.
//
// For v2 channels:
// The single schnorr signature signs the tlv fields of the v2 channel
// announcement message which are in the signed range.
type ChannelAuthProof struct {
	// Version is the version of the channel announcement.
	Version lnwire.GossipVersion

	// NodeSig1Bytes are the raw bytes of the first node signature encoded
	// in DER format.
	//
	// NOTE: v1 channel announcements only.
	NodeSig1Bytes fn.Option[[]byte]

	// NodeSig2Bytes are the raw bytes of the second node signature
	// encoded in DER format.
	//
	// NOTE: v1 channel announcements only.
	NodeSig2Bytes fn.Option[[]byte]

	// BitcoinSig1Bytes are the raw bytes of the first bitcoin signature
	// encoded in DER format.
	//
	// NOTE: v1 channel announcements only.
	BitcoinSig1Bytes fn.Option[[]byte]

	// BitcoinSig2Bytes are the raw bytes of the second bitcoin signature
	// encoded in DER format.
	//
	// NOTE: v1 channel announcements only.
	BitcoinSig2Bytes fn.Option[[]byte]

	// Signature is the raw bytes of the single schnorr signature for v2
	// channel announcements.
	//
	// NOTE: v2 channel announcements only.
	Signature fn.Option[[]byte]
}

// IsEmpty check is the authentication proof is empty Proof is empty.
func (c *ChannelAuthProof) IsEmpty() bool {
	// For v1 channel announcements, we either have all four signatures or
	// none.
	if c.Version == lnwire.GossipVersion1 {
		return c.NodeSig1Bytes.IsNone()
	}

	// For v2 channel announcements, we only have a single signature.
	return c.Signature.IsNone()
}

// NewV1ChannelAuthProof creates a new ChannelAuthProof for a v1 channel
// announcement.
func NewV1ChannelAuthProof(nodeSig1, nodeSig2, bitcoinSig1,
	bitcoinSig2 []byte) *ChannelAuthProof {

	return &ChannelAuthProof{
		Version:          lnwire.GossipVersion1,
		NodeSig1Bytes:    fn.Some(nodeSig1),
		NodeSig2Bytes:    fn.Some(nodeSig2),
		BitcoinSig1Bytes: fn.Some(bitcoinSig1),
		BitcoinSig2Bytes: fn.Some(bitcoinSig2),
	}
}

// NewV2ChannelAuthProof creates a new ChannelAuthProof for a v2 channel
// announcement.
func NewV2ChannelAuthProof(signature []byte) *ChannelAuthProof {
	return &ChannelAuthProof{
		Version:   lnwire.GossipVersion2,
		Signature: fn.Some(signature),
	}
}

// NodeSig1 returns the first node signature bytes, or nil if not present.
func (c *ChannelAuthProof) NodeSig1() []byte {
	return c.NodeSig1Bytes.UnwrapOr(nil)
}

// NodeSig2 returns the second node signature bytes, or nil if not present.
func (c *ChannelAuthProof) NodeSig2() []byte {
	return c.NodeSig2Bytes.UnwrapOr(nil)
}

// BitcoinSig1 returns the first bitcoin signature bytes, or nil if not
// present.
func (c *ChannelAuthProof) BitcoinSig1() []byte {
	return c.BitcoinSig1Bytes.UnwrapOr(nil)
}

// BitcoinSig2 returns the second bitcoin signature bytes, or nil if not
// present.
func (c *ChannelAuthProof) BitcoinSig2() []byte {
	return c.BitcoinSig2Bytes.UnwrapOr(nil)
}

// Sig returns the v2 signature bytes, or nil if not present.
func (c *ChannelAuthProof) Sig() []byte {
	return c.Signature.UnwrapOr(nil)
}
