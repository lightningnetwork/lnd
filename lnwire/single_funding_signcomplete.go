package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
)

// SingleFundingSignComplete is the message Bob sends to Alice which delivers
// a signature for Alice's version of the commitment transaction. After thie
// message is received and processed by Alice, she is free to broadcast the
// funding transaction.
type SingleFundingSignComplete struct {
	// ChannelID serves to uniquely identify the future channel created by
	// the initiated single funder workflow.
	ChannelID uint64

	// CommitmentSignature is Bobs's signature for Alice's version of the
	// commitment transaction.
	CommitmentSignature *btcec.Signature
}

// Decode deserializes the serialized SingleFundingSignComplete stored in the
// passed io.Reader into the target SingleFundingComplete using the
// deserialization rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// CommitmentSignature (73)
	err := readElements(r,
		&c.ChannelID,
		&c.CommitmentSignature)
	if err != nil {
		return err
	}

	return nil
}

// NewSingleFundingSignComplete creates a new empty SingleFundingSignComplete
// message.
func NewSingleFundingSignComplete() *SingleFundingSignComplete {
	return &SingleFundingSignComplete{}
}

// Encode serializes the target SingleFundingSignComplete into the passed
// io.Writer implementation. Serialization will observe the rules defined by
// the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.CommitmentSignature)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the uint32 code which uniquely identifies this message as a
// SingleFundingSignComplete on the wire.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) Command() uint32 {
	return CmdSingleFundingSignComplete
}

// MaxPayloadLength returns the maximum allowed payload length for a
// SingleFundingComplete. This is calculated by summing the max length of all
// the fields within a SingleFundingResponse. The final breakdown
// is: 8 + 73 = 81
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) MaxPayloadLength(uint32) uint32 {
	return 81
}

// Validate examines each populated field within the SingleFundingComplete for
// field sanity.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingSignComplete) Validate() error {
	if s.CommitmentSignature == nil {
		return fmt.Errorf("commitment signature must be non-nil")
	}

	// We're good!
	return nil
}

// String returns the string representation of the SingleFundingSignComplete.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingSignComplete) String() string {
	return fmt.Sprintf("\n--- Begin FundingSignComplete ---\n") +
		fmt.Sprintf("ChannelID:\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("CommitmentSignature\t\t%s\n", c.CommitmentSignature) +
		fmt.Sprintf("--- End FundingSignComplete ---\n")
}
