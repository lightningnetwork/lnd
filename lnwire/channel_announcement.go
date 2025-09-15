package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// ChannelAnnouncement1 message is used to announce the existence of a channel
// between two peers in the overlay, which is propagated by the discovery
// service over broadcast handler.
type ChannelAnnouncement1 struct {
	// This signatures are used by nodes in order to create cross
	// references between node's channel and node. Requiring both nodes
	// to sign indicates they are both willing to route other payments via
	// this node.
	NodeSig1 Sig
	NodeSig2 Sig

	// This signatures are used by nodes in order to create cross
	// references between node's channel and node. Requiring the bitcoin
	// signatures proves they control the channel.
	BitcoinSig1 Sig
	BitcoinSig2 Sig

	// Features is the feature vector that encodes the features supported
	// by the target node. This field can be used to signal the type of the
	// channel, or modifications to the fields that would normally follow
	// this vector.
	Features *RawFeatureVector

	// ChainHash denotes the target chain that this channel was opened
	// within. This value should be the genesis hash of the target chain.
	ChainHash chainhash.Hash

	// ShortChannelID is the unique description of the funding transaction,
	// or where exactly it's located within the target blockchain.
	ShortChannelID ShortChannelID

	// The public keys of the two nodes who are operating the channel, such
	// that is NodeID1 the numerically-lesser than NodeID2 (ascending
	// numerical order).
	NodeID1 [33]byte
	NodeID2 [33]byte

	// Public keys which corresponds to the keys which was declared in
	// multisig funding transaction output.
	BitcoinKey1 [33]byte
	BitcoinKey2 [33]byte

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData ExtraOpaqueData
}

// A compile time check to ensure ChannelAnnouncement implements the
// lnwire.Message interface.
var _ Message = (*ChannelAnnouncement1)(nil)

// A compile time check to ensure ChannelAnnouncement1 implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*ChannelAnnouncement1)(nil)

// Decode deserializes a serialized ChannelAnnouncement stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement1) Decode(r io.Reader, _ uint32) error {
	err := ReadElements(r,
		&a.NodeSig1,
		&a.NodeSig2,
		&a.BitcoinSig1,
		&a.BitcoinSig2,
		&a.Features,
		a.ChainHash[:],
		&a.ShortChannelID,
		&a.NodeID1,
		&a.NodeID2,
		&a.BitcoinKey1,
		&a.BitcoinKey2,
		&a.ExtraOpaqueData,
	)
	if err != nil {
		return err
	}

	return a.ExtraOpaqueData.ValidateTLV()
}

// Encode serializes the target ChannelAnnouncement into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement1) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteSig(w, a.NodeSig1); err != nil {
		return err
	}

	if err := WriteSig(w, a.NodeSig2); err != nil {
		return err
	}

	if err := WriteSig(w, a.BitcoinSig1); err != nil {
		return err
	}

	if err := WriteSig(w, a.BitcoinSig2); err != nil {
		return err
	}

	if err := WriteRawFeatureVector(w, a.Features); err != nil {
		return err
	}

	if err := WriteBytes(w, a.ChainHash[:]); err != nil {
		return err
	}

	if err := WriteShortChannelID(w, a.ShortChannelID); err != nil {
		return err
	}

	if err := WriteBytes(w, a.NodeID1[:]); err != nil {
		return err
	}

	if err := WriteBytes(w, a.NodeID2[:]); err != nil {
		return err
	}

	if err := WriteBytes(w, a.BitcoinKey1[:]); err != nil {
		return err
	}

	if err := WriteBytes(w, a.BitcoinKey2[:]); err != nil {
		return err
	}

	return WriteBytes(w, a.ExtraOpaqueData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement1) MsgType() MessageType {
	return MsgChannelAnnouncement
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (a *ChannelAnnouncement1) SerializedSize() (uint32, error) {
	return MessageSerializedSize(a)
}

// DataToSign is used to retrieve part of the announcement message which should
// be signed.
func (a *ChannelAnnouncement1) DataToSign() ([]byte, error) {
	// We should not include the signatures itself.
	b := make([]byte, 0, MaxMsgBody)
	buf := bytes.NewBuffer(b)

	if err := WriteRawFeatureVector(buf, a.Features); err != nil {
		return nil, err
	}

	if err := WriteBytes(buf, a.ChainHash[:]); err != nil {
		return nil, err
	}

	if err := WriteShortChannelID(buf, a.ShortChannelID); err != nil {
		return nil, err
	}

	if err := WriteBytes(buf, a.NodeID1[:]); err != nil {
		return nil, err
	}

	if err := WriteBytes(buf, a.NodeID2[:]); err != nil {
		return nil, err
	}

	if err := WriteBytes(buf, a.BitcoinKey1[:]); err != nil {
		return nil, err
	}

	if err := WriteBytes(buf, a.BitcoinKey2[:]); err != nil {
		return nil, err
	}

	if err := WriteBytes(buf, a.ExtraOpaqueData); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Node1KeyBytes returns the bytes representing the public key of node 1 in the
// channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (a *ChannelAnnouncement1) Node1KeyBytes() [33]byte {
	return a.NodeID1
}

// Node2KeyBytes returns the bytes representing the public key of node 2 in the
// channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (a *ChannelAnnouncement1) Node2KeyBytes() [33]byte {
	return a.NodeID2
}

// GetChainHash returns the hash of the chain which this channel's funding
// transaction is confirmed in.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (a *ChannelAnnouncement1) GetChainHash() chainhash.Hash {
	return a.ChainHash
}

// SCID returns the short channel ID of the channel.
//
// NOTE: This is part of the ChannelAnnouncement interface.
func (a *ChannelAnnouncement1) SCID() ShortChannelID {
	return a.ShortChannelID
}

// GossipVersion returns the gossip version that this message is part of.
//
// NOTE: this is part of the GossipMessage interface.
func (a *ChannelAnnouncement1) GossipVersion() GossipVersion {
	return GossipVersion1
}

// A compile-time check to ensure that ChannelAnnouncement1 implements the
// ChannelAnnouncement interface.
var _ ChannelAnnouncement = (*ChannelAnnouncement1)(nil)
