package lnwire

import "io"

// ChannelReestablish is a message sent between peers that have an existing
// open channel upon connection reestablishment. This message allows both sides
// to report their local state, and their current knowledge of the state of the
// remote commitment chain. If a deviation is detected and can be recovered
// from, then the necessary messages will be retransmitted. If the level of
// desynchronization if irreconcilable, then the channel will be force closed.
type ChannelReestablish struct {
	// ChanID is the channel ID of the channel state we're attempting
	// synchronize with the remote party.
	ChanID ChannelID

	// NextLocalCommitHeight is the next local commitment height of the
	// sending party. If the height of the sender's commitment chain from
	// the receiver's Pov is one less that this number, then the sender
	// should re-send the *exact* same proposed commitment.
	//
	// In other words, the receiver should re-send their last sent
	// commitment iff:
	//
	//  * NextLocalCommitHeight == remoteCommitChain.Height
	//
	// This covers the case of a lost commitment which was sent by the
	// sender of this message, but never received by the receiver of this
	// message.
	NextLocalCommitHeight uint64

	// RemoteCommitTailHeight is the height of the receiving party's
	// unrevoked commitment from the PoV of the sender of this message. If
	// the height of the receiver's commitment is *one more* than this
	// value, then their prior RevokeAndAck message should be
	// retransmitted.
	//
	// In other words, the receiver should re-send their last sent
	// RevokeAndAck message iff:
	//
	//  * localCommitChain.tail().Height == RemoteCommitTailHeight + 1
	//
	// This covers the case of a lost revocation, wherein the receiver of
	// the message sent a revocation for a prior state, but the sender of
	// the message never fully processed it.
	RemoteCommitTailHeight uint64
}

// A compile time check to ensure ChannelReestablish implements the
// lnwire.Message interface.
var _ Message = (*ChannelReestablish)(nil)

// Encode serializes the target ChannelReestablish into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelReestablish) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		a.ChanID,
		a.NextLocalCommitHeight,
		a.RemoteCommitTailHeight,
	)
}

// Decode deserializes a serialized ChannelReestablish stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelReestablish) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&a.ChanID,
		&a.NextLocalCommitHeight,
		&a.RemoteCommitTailHeight,
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

	// NextLocalCommitHeight - 8 bytes
	length += 8

	// RemoteCommitTailHeight - 8 bytes
	length += 8

	return length
}
