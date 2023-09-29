package lnwire

import (
	"bytes"
	"io"
)

// AnnouncementSignatures2 is a direct message between two endpoints of a
// channel and serves as an opt-in mechanism to allow the announcement of
// a taproot channel to the rest of the network. It contains the necessary
// signatures by the sender to construct the channel_announcement_ message.
type AnnouncementSignatures2 struct {
	// ChannelID is the unique description of the funding transaction.
	// Channel id is better for users and debugging and short channel id is
	// used for quick test on existence of the particular utxo inside the
	// blockchain, because it contains information about block.
	ChannelID ChannelID

	// ShortChannelID is the unique description of the funding transaction.
	// It is constructed with the most significant 3 bytes as the block
	// height, the next 3 bytes indicating the transaction index within the
	// block, and the least significant two bytes indicating the output
	// index which pays to the channel.
	ShortChannelID ShortChannelID

	// PartialSignature is the combination of the partial Schnorr signature
	// created for the node's bitcoin key with the partial signature created
	// for the node's node ID key.
	PartialSignature PartialSig

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData ExtraOpaqueData
}

// A compile time check to ensure AnnouncementSignatures2 implements the
// lnwire.Message interface.
var _ Message = (*AnnouncementSignatures2)(nil)

// Decode deserializes a serialized AnnounceSignatures stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *AnnouncementSignatures2) Decode(r io.Reader, _ uint32) error {
	return ReadElements(r,
		&a.ChannelID,
		&a.ShortChannelID,
		&a.PartialSignature,
		&a.ExtraOpaqueData,
	)
}

// Encode serializes the target AnnounceSignatures into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *AnnouncementSignatures2) Encode(w *bytes.Buffer, _ uint32) error {
	return WriteElements(w,
		a.ChannelID,
		a.ShortChannelID,
		a.PartialSignature,
		a.ExtraOpaqueData,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *AnnouncementSignatures2) MsgType() MessageType {
	return MsgAnnouncementSignatures2
}
