package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
)

// RevokeAndAck is sent by either side once a CommitSig message has been
// received, and validated. This message serves to revoke the prior commitment
// transaction, which was the most up to date version until a CommitSig message
// referencing the specified ChannelPoint was received.  Additionally, this
// message also piggyback's the next revocation hash that Alice should use when
// constructing the Bob's version of the next commitment transaction (which
// would be done before sending a CommitSig message).  This piggybacking allows
// Alice to send the next CommitSig message modifying Bob's commitment
// transaction without first asking for a revocation hash initially.
type RevokeAndAck struct {
	// ChanID uniquely identifies to which currently active channel this
	// RevokeAndAck applies to.
	ChanID ChannelID

	// Revocation is the preimage to the revocation hash of the now prior
	// commitment transaction.
	Revocation [32]byte

	// NextRevocationKey is the next commitment point which should be used
	// for the next commitment transaction the remote peer creates for us.
	// This, in conjunction with revocation base point will be used to
	// create the proper revocation key used within the commitment
	// transaction.
	NextRevocationKey *btcec.PublicKey
}

// NewRevokeAndAck creates a new RevokeAndAck message.
func NewRevokeAndAck() *RevokeAndAck {
	return &RevokeAndAck{}
}

// A compile time check to ensure RevokeAndAck implements the lnwire.Message
// interface.
var _ Message = (*RevokeAndAck)(nil)

// Decode deserializes a serialized RevokeAndAck message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&c.ChanID,
		c.Revocation[:],
		&c.NextRevocationKey,
	)
}

// Encode serializes the target RevokeAndAck into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		c.ChanID,
		c.Revocation[:],
		c.NextRevocationKey,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) MsgType() MessageType {
	return MsgRevokeAndAck
}

// MaxPayloadLength returns the maximum allowed payload size for a RevokeAndAck
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) MaxPayloadLength(uint32) uint32 {
	// 32 + 32 + 33
	return 97
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (c *RevokeAndAck) TargetChanID() ChannelID {
	return c.ChanID
}
