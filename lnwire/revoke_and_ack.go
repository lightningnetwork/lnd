package lnwire

import (
	"io"

	"github.com/roasbeef/btcd/btcec"
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
	//
	// If the received revocation is the all zeroes hash ('0' * 32), then
	// this RevokeAndAck is being sent in order to build up the sender's
	// initial revocation window (IRW). In this case, the RevokeAndAck
	// should be added to the receiver's queue of unused revocations to be
	// used to construct future commitment transactions.
	Revocation [32]byte

	// NextRevocationKey is the next revocation key which should be added
	// to the queue of unused revocation keys for the remote peer. This key
	// will be used within the revocation clause for any new commitment
	// transactions created for the remote peer.
	NextRevocationKey *btcec.PublicKey

	// NextRevocationHash is the next revocation hash which should be added
	// to the queue on unused revocation hashes for the remote peer. This
	// revocation hash will be used within any HTLCs included within this
	// next commitment transaction.
	NextRevocationHash [32]byte
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
	return readElements(r,
		&c.ChanID,
		c.Revocation[:],
		&c.NextRevocationKey,
		c.NextRevocationHash[:],
	)
}

// Encode serializes the target RevokeAndAck into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.ChanID,
		c.Revocation[:],
		c.NextRevocationKey,
		c.NextRevocationHash[:],
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) MsgType() MessageType {
	return MsgRevokeAndAck
}

// MaxPayloadLength returns the maximum allowed payload size for a
// RevokeAndAck complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) MaxPayloadLength(uint32) uint32 {
	// 32 + 32 + 33 + 32
	return 129
}
