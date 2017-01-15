package lnwire

import "io"

// SingleFundingOpenProof is the message sent by the channel initiator to the
// responder after the previously constructed funding transaction has achieved
// a sufficient number of confirmations. It is the initiator's duty to present
// a proof of an open channel to the responder. Otherwise, responding node may
// be vulnerable to a resource exhaustion attack wherein the requesting node
// repeatedly negotiates funding transactions which are never broadcast. If the
// responding node commits resources to watch the chain for each funding
// transaction, then this attack is very cheap for the initiator.
type SingleFundingOpenProof struct {
	// ChannelID serves to uniquely identify the future channel created by
	// the initiated single funder workflow.
	ChannelID uint64

	// ChanChainID is the channel ID of the completed channel which
	// compactly encodes the location of the channel within the current
	// main chain.
	ChanChainID ChannelID
}

// NewSingleFundingSignComplete creates a new empty SingleFundingOpenProof
// message.
func NewSingleFundingOpenProof(chanID uint64, chainID ChannelID) *SingleFundingOpenProof {
	return &SingleFundingOpenProof{
		ChannelID:   chanID,
		ChanChainID: chainID,
	}
}

// Decode deserializes the serialized SingleFundingOpenProof stored in the
// passed io.Reader into the target SingleFundingOpenProof using the
// deserialization rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingOpenProof) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// ChanChainID (8)
	err := readElements(r,
		&s.ChannelID,
		&s.ChanChainID)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target SingleFundingOpenProof into the passed
// io.Writer implementation. Serialization will observe the rules defined by
// the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingOpenProof) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		s.ChannelID,
		s.ChanChainID)
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
