package lnwire

import (
	"fmt"
	"io"
)

// SingleFundingOpenProof is the message sent by the channel initiator to the
// responder after the previously constructed funding transaction has achieved
// a sufficient number of confirmations. It is the initiator's duty to present
// a proof of an open channel to the responder. Otherwise, responding node may
// be vulernable to a resource exhasution attack wherein the a requesting node
// repeatedly negotiates funding transactions which are never broadcast. If the
// responding node commits resources to watch the chain for each funding
// transaction, then this attack is very cheap for the initiator.
type SingleFundingOpenProof struct {
	// ChannelID serves to uniquely identify the future channel created by
	// the initiated single funder workflow.
	ChannelID uint64

	// SpvProof is an merkle proof of the inclusion of the funding
	// transaction within a block.
	// TODO(roasbeef): spec out format for SPV proof, only of single tx so
	// can be compact
	SpvProof []byte
}

// Decode deserializes the serialized SingleFundingOpenProof stored in the
// passed io.Reader into the target SingleFundingOpenProof using the
// deserialization rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingOpenProof) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// SpvProof (?)
	err := readElements(r,
		&s.ChannelID,
		&s.SpvProof)
	if err != nil {
		return err
	}

	return nil
}

// NewSingleFundingSignComplete creates a new empty SingleFundingOpenProof
// message.
func NewSingleFundingOpenProof() *SingleFundingOpenProof {
	return &SingleFundingOpenProof{}
}

// Encode serializes the target SingleFundingOpenProof into the passed
// io.Writer implementation. Serialization will observe the rules defined by
// the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingOpenProof) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		s.ChannelID,
		s.SpvProof)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the uint32 code which uniquely identifies this message as a
// SingleFundingSignComplete on the wire.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingOpenProof) Command() uint32 {
	return CmdSingleFundingOpenProof
}

// MaxPayloadLength returns the maximum allowed payload length for a
// SingleFundingComplete. This is calculated by summing the max length of all
// the fields within a SingleFundingOpenProof.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingOpenProof) MaxPayloadLength(uint32) uint32 {
	// 8 +
	return 81
}

// Validate examines each populated field within the SingleFundingOpenProof for
// field sanity.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingOpenProof) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the SingleFundingOpenProof.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingOpenProof) String() string {
	return fmt.Sprintf("\n--- Begin FundingSignComplete ---\n") +
		fmt.Sprintf("ChannelID:\t\t%d\n", s.ChannelID) +
		fmt.Sprintf("SpvProof\t\t%s\n", s.SpvProof) +
		fmt.Sprintf("--- End FundingSignComplete ---\n")
}
