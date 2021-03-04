package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec"
)

// ChannelReestablish is a message sent between peers that have an existing
// open channel upon connection reestablishment. This message allows both sides
// to report their local state, and their current knowledge of the state of the
// remote commitment chain. If a deviation is detected and can be recovered
// from, then the necessary messages will be retransmitted. If the level of
// desynchronization is irreconcilable, then the channel will be force closed.
type ChannelReestablish struct {
	// ChanID is the channel ID of the channel state we're attempting to
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

	// LastRemoteCommitSecret is the last commitment secret that the
	// receiving node has sent to the sending party. This will be the
	// secret of the last revoked commitment transaction. Including this
	// provides proof that the sending node at least knows of this state,
	// as they couldn't have produced it if it wasn't sent, as the value
	// can be authenticated by querying the shachain or the receiving
	// party.
	LastRemoteCommitSecret [32]byte

	// LocalUnrevokedCommitPoint is the commitment point used in the
	// current un-revoked commitment transaction of the sending party.
	LocalUnrevokedCommitPoint *btcec.PublicKey
}

// A compile time check to ensure ChannelReestablish implements the
// lnwire.Message interface.
var _ Message = (*ChannelReestablish)(nil)

// Encode serializes the target ChannelReestablish into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelReestablish) Encode(w io.Writer, pver uint32) error {
	err := WriteElements(w,
		a.ChanID,
		a.NextLocalCommitHeight,
		a.RemoteCommitTailHeight,
	)
	if err != nil {
		return err
	}

	// If the commit point wasn't sent, then we won't write out any of the
	// remaining fields as they're optional.
	if a.LocalUnrevokedCommitPoint == nil {
		return nil
	}

	// Otherwise, we'll write out the remaining elements.
	return WriteElements(w, a.LastRemoteCommitSecret[:],
		a.LocalUnrevokedCommitPoint)
}

// Decode deserializes a serialized ChannelReestablish stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelReestablish) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(r,
		&a.ChanID,
		&a.NextLocalCommitHeight,
		&a.RemoteCommitTailHeight,
	)
	if err != nil {
		return err
	}

	// This message has currently defined optional fields. As a result,
	// we'll only proceed if there's still bytes remaining within the
	// reader.
	//
	// We'll manually parse out the optional fields in order to be able to
	// still utilize the io.Reader interface.

	// We'll first attempt to read the optional commit secret, if we're at
	// the EOF, then this means the field wasn't included so we can exit
	// early.
	var buf [32]byte
	_, err = io.ReadFull(r, buf[:32])
	if err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}

	// If the field is present, then we'll copy it over and proceed.
	copy(a.LastRemoteCommitSecret[:], buf[:])

	// We'll conclude by parsing out the commitment point. We don't check
	// the error in this case, as it has included the commit secret, then
	// they MUST also include the commit point.
	return ReadElement(r, &a.LocalUnrevokedCommitPoint)
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

	// LastRemoteCommitSecret - 32 bytes
	length += 32

	// LocalUnrevokedCommitPoint - 33 bytes
	length += 33

	return length
}
