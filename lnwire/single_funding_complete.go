package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
)

// SingleFundingComplete is the message Alice sends to Bob once she is able to
// fully assemble the funding transaction, and both versions of the commitment
// transaction. The purpose of this message is to present Bob with the items
// required for him to generate a signature for Alice's version of the
// commitment transaction.
type SingleFundingComplete struct {
	// ChannelID serves to uniquely identify the future channel created by
	// the initiated single funder workflow.
	ChannelID uint64

	// FundingOutPoint is the outpoint (txid:index) of the funding
	// transaction. With this value, Bob will be able to generate a
	// signature for Alice's version of the commitment transaction.
	FundingOutPoint wire.OutPoint

	// CommitmentSignature is Alice's signature for Bob's version of the
	// commitment transaction.
	CommitmentSignature *btcec.Signature
}

// Decode deserializes the serialized SingleFundingComplete stored in the passed
// io.Reader into the target SingleFundingComplete using the deserialization
// rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// FundingOutPoint (36)
	// Commitment Signature (73)
	err := readElements(r,
		&s.ChannelID,
		&s.FundingOutPoint,
		&s.CommitmentSignature)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target SingleFundingComplete into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) Encode(w io.Writer, pver uint32) error {
	// ChannelID (8)
	// FundingOutPoint (36)
	// Commitment Signature (73)
	err := writeElements(w,
		s.ChannelID,
		s.FundingOutPoint,
		s.CommitmentSignature)
	if err != nil {
		return err
	}

	return nil
}

// NewSingleFundingComplete creates, and returns a new empty
// SingleFundingResponse.
func NewSingleFundingComplete() *SingleFundingComplete {
	return &SingleFundingComplete{}
}

// Command returns the uint32 code which uniquely identifies this message as a
// SingleFundingRequest on the wire.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) Command() uint32 {
	return CmdSingleFundingComplete
}

// MaxPayloadLength returns the maximum allowed payload length for a
// SingleFundingComplete. This is calculated by summing the max length of all
// the fields within a SingleFundingResponse. To enforce a maximum
// DeliveryPkScript size, the size of a P2PKH public key script is used.
// Therefore, the final breakdown is: 8 + 36 + 73 = 117
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) MaxPayloadLength(uint32) uint32 {
	return 117
}

// Validate examines each populated field within the SingleFundingComplete for
// field sanity.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) Validate() error {
	var zeroHash [32]byte
	if bytes.Equal(zeroHash[:], s.FundingOutPoint.Hash[:]) {
		return fmt.Errorf("funding outpoint hash must be non-zero")
	}

	if s.CommitmentSignature == nil {
		return fmt.Errorf("commitment signature must be non-nil")
	}

	// We're good!
	return nil
}

// String returns the string representation of the SingleFundingResponse.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) String() string {
	return fmt.Sprintf("\n--- Begin SingleFundingComplete ---\n") +
		fmt.Sprintf("ChannelID:\t\t\t%d\n", s.ChannelID) +
		fmt.Sprintf("FundingOutPoint:\t\t\t%x\n", s.FundingOutPoint) +
		fmt.Sprintf("CommitmentSignature\t\t\t\t%x\n", s.CommitmentSignature) +
		fmt.Sprintf("--- End SingleFundingComplete ---\n")
}
