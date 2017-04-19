package lnwire

import (
	"io"

	"github.com/roasbeef/btcd/btcec"
)

// SingleFundingSignComplete is the message Bob sends to Alice which delivers
// a signature for Alice's version of the commitment transaction. After this
// message is received and processed by Alice, she is free to broadcast the
// funding transaction.
type SingleFundingSignComplete struct {
	// PendingChannelID serves to uniquely identify the future channel
	// created by the initiated single funder workflow.
	PendingChannelID [32]byte

	// CommitSignature is Bobs's signature for Alice's version of the
	// commitment transaction.
	CommitSignature *btcec.Signature
}

// NewSingleFundingSignComplete creates a new empty SingleFundingSignComplete
// message.
func NewSingleFundingSignComplete(chanID [32]byte,
	sig *btcec.Signature) *SingleFundingSignComplete {

	return &SingleFundingSignComplete{
		PendingChannelID: chanID,
		CommitSignature:  sig,
	}
}

// Decode deserializes the serialized SingleFundingSignComplete stored in the
// passed io.Reader into the target SingleFundingComplete using the
// deserialization rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		c.PendingChannelID[:],
		&c.CommitSignature)
}

// Encode serializes the target SingleFundingSignComplete into the passed
// io.Writer implementation. Serialization will observe the rules defined by
// the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.PendingChannelID[:],
		c.CommitSignature)
}

// MsgType returns the uint32 code which uniquely identifies this message as a
// SingleFundingSignComplete on the wire.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) MsgType() MessageType {
	return MsgSingleFundingSignComplete
}

// MaxPayloadLength returns the maximum allowed payload length for a
// SingleFundingSignComplete. This is calculated by summing the max length of
// all the fields within a SingleFundingSignComplete.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) MaxPayloadLength(uint32) uint32 {
	// 32 + 64
	return 96
}
