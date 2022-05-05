package lnwire

import (
	"io"
)

// OpaqueReason is an opaque encrypted byte slice that encodes the exact
// failure reason and additional some supplemental data. The contents of this
// slice can only be decrypted by the sender of the original HTLC.
type OpaqueReason []byte

// UpdateFailHTLC is sent by Alice to Bob in order to remove a previously added
// HTLC. Upon receipt of an UpdateFailHTLC the HTLC should be removed from the
// next commitment transaction, with the UpdateFailHTLC propagated backwards in
// the route to fully undo the HTLC.
type UpdateFailHTLC struct {
	// ChanID is the particular active channel that this
	// UpdateFailHTLC is bound to.
	ChanID ChannelID

	// ID references which HTLC on the remote node's commitment transaction
	// has timed out.
	ID uint64

	// Reason is an onion-encrypted blob that details why the HTLC was
	// failed. This blob is only fully decryptable by the initiator of the
	// HTLC message.
	Reason OpaqueReason
}

// A compile time check to ensure UpdateFailHTLC implements the lnwire.Message
// interface.
var _ Message = (*UpdateFailHTLC)(nil)

// Decode deserializes a serialized UpdateFailHTLC message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&c.ChanID,
		&c.ID,
		&c.Reason,
	)
}

// Encode serializes the target UpdateFailHTLC into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		c.ChanID,
		c.ID,
		c.Reason,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) MsgType() MessageType {
	return MsgUpdateFailHTLC
}

// MaxPayloadLength returns the maximum allowed payload size for an UpdateFailHTLC
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) MaxPayloadLength(uint32) uint32 {
	var length uint32

	// Length of the ChanID
	length += 32

	// Length of the ID
	length += 8

	// Length of the length opaque reason
	length += 2

	// Length of the Reason
	length += 292

	return length
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (c *UpdateFailHTLC) TargetChanID() ChannelID {
	return c.ChanID
}
