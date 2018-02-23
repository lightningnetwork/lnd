package lnwire

import "io"

// AnnounceSignatures this is a direct message between two endpoints of a
// channel and serves as an opt-in mechanism to allow the announcement of
// the channel to the rest of the network. It contains the necessary
// signatures by the sender to construct the channel announcement message.
type AnnounceSignatures struct {
	// ChannelID is the unique description of the funding transaction.
	// Channel id is better for users and debugging and short channel id is
	// used for quick test on existence of the particular utxo inside the
	// block chain, because it contains information about block.
	ChannelID ChannelID

	// ShortChannelID is the unique description of the funding
	// transaction. It is constructed with the most significant 3 bytes
	// as the block height, the next 3 bytes indicating the transaction
	// index within the block, and the least significant two bytes
	// indicating the output index which pays to the channel.
	ShortChannelID ShortChannelID

	// NodeSignature is the signature which contains the signed announce
	// channel message, by this signature we proof that we possess of the
	// node pub key and creating the reference node_key -> bitcoin_key.
	NodeSignature Sig

	// BitcoinSignature is the signature which contains the signed node
	// public key, by this signature we proof that we possess of the
	// bitcoin key and and creating the reverse reference bitcoin_key ->
	// node_key.
	BitcoinSignature Sig
}

// A compile time check to ensure AnnounceSignatures implements the
// lnwire.Message interface.
var _ Message = (*AnnounceSignatures)(nil)

// Decode deserializes a serialized AnnounceSignatures stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *AnnounceSignatures) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&a.ChannelID,
		&a.ShortChannelID,
		&a.NodeSignature,
		&a.BitcoinSignature,
	)
}

// Encode serializes the target AnnounceSignatures into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *AnnounceSignatures) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		a.ChannelID,
		a.ShortChannelID,
		a.NodeSignature,
		a.BitcoinSignature,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *AnnounceSignatures) MsgType() MessageType {
	return MsgAnnounceSignatures
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *AnnounceSignatures) MaxPayloadLength(pver uint32) uint32 {
	var length uint32

	// ChannelID - 36 bytes
	length += 36

	// ShortChannelID - 8 bytes
	length += 8

	// NodeSignatures - 64 bytes
	length += 64

	// BitcoinSignatures - 64 bytes
	length += 64

	return length
}
