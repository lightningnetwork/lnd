package lnwire

import "io"

// ChannelReestablish is sent during node reconnection for every channel
// established in order to synchronize the states on both sides.
type ChannelReestablish struct {
	// ChanID serves to identify to which channel this message belongs.
	ChanID ChannelID

	// NextLocalCommitmentNumber is the commitment number of the next
	// commitment signed message it expects to receive.
	NextLocalCommitmentNumber uint64

	// NextRemoteRevocationNumber is the commitment number of the next
	// revoke and ack message it expects to receive.
	NextRemoteRevocationNumber uint64
}

// A compile time check to ensure ChannelReestablish implements the
// lnwire.Message interface.
var _ Message = (*ChannelReestablish)(nil)

// Decode deserializes a serialized ChannelReestablish stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelReestablish) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&a.ChanID,
		&a.NextLocalCommitmentNumber,
		&a.NextRemoteRevocationNumber,
	)
}

// Encode serializes the target ChannelReestablish into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelReestablish) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		a.ChanID,
		a.NextLocalCommitmentNumber,
		a.NextRemoteRevocationNumber,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *ChannelReestablish) MsgType() MessageType {
	return MsgChannelReestablish
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelReestablish) MaxPayloadLength(pver uint32) uint32 {
	var length uint32

	// ChanID - 32 bytes
	length += 32

	// NextLocalCommitmentNumber - 8 bytes
	length += 8

	// NextRemoteRevocationNumber - 8 bytes
	length += 8

	return length
}
