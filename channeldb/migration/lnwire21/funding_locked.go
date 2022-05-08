package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
)

// FundingLocked is the message that both parties to a new channel creation
// send once they have observed the funding transaction being confirmed on the
// blockchain. FundingLocked contains the signatures necessary for the channel
// participants to advertise the existence of the channel to the rest of the
// network.
type FundingLocked struct {
	// ChanID is the outpoint of the channel's funding transaction. This
	// can be used to query for the channel in the database.
	ChanID ChannelID

	// NextPerCommitmentPoint is the secret that can be used to revoke the
	// next commitment transaction for the channel.
	NextPerCommitmentPoint *btcec.PublicKey
}

// NewFundingLocked creates a new FundingLocked message, populating it with the
// necessary IDs and revocation secret.
func NewFundingLocked(cid ChannelID, npcp *btcec.PublicKey) *FundingLocked {
	return &FundingLocked{
		ChanID:                 cid,
		NextPerCommitmentPoint: npcp,
	}
}

// A compile time check to ensure FundingLocked implements the lnwire.Message
// interface.
var _ Message = (*FundingLocked)(nil)

// Decode deserializes the serialized FundingLocked message stored in the
// passed io.Reader into the target FundingLocked using the deserialization
// rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&c.ChanID,
		&c.NextPerCommitmentPoint)
}

// Encode serializes the target FundingLocked message into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		c.ChanID,
		c.NextPerCommitmentPoint)
}

// MsgType returns the uint32 code which uniquely identifies this message as a
// FundingLocked message on the wire.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) MsgType() MessageType {
	return MsgFundingLocked
}

// MaxPayloadLength returns the maximum allowed payload length for a
// FundingLocked message. This is calculated by summing the max length of all
// the fields within a FundingLocked message.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) MaxPayloadLength(uint32) uint32 {
	var length uint32

	// ChanID - 32 bytes
	length += 32

	// NextPerCommitmentPoint - 33 bytes
	length += 33

	// 65 bytes
	return length
}
