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
	// PendingChannelID serves to uniquely identify the future channel
	// created by the initiated single funder workflow.
	PendingChannelID [32]byte

	// FundingOutPoint is the outpoint (txid:index) of the funding
	// transaction. With this value, Bob will be able to generate a
	// signature for Alice's version of the commitment transaction.
	FundingOutPoint wire.OutPoint

	// CommitSignature is Alice's signature for Bob's version of the
	// commitment transaction.
	CommitSignature *btcec.Signature

	// RevocationKey is the initial key to be used for the revocation
	// clause within the self-output of the initiators's commitment
	// transaction. Once an initial new state is created, the initiator
	// will send a preimage which will allow the initiator to sweep the
	// initiator's funds if the violate the contract.
	RevocationKey *btcec.PublicKey

	// StateHintObsfucator is the set of bytes used by the initiator to
	// obsfucate the state number encoded within the sequence number for
	// the commitment transaction's only input. The initiator generates
	// this value by hashing the 0th' state derived from the revocation PRF
	// an additional time.
	StateHintObsfucator [6]byte
}

// NewSingleFundingComplete creates, and returns a new empty
// SingleFundingResponse.
func NewSingleFundingComplete(pChanID [32]byte, fundingPoint wire.OutPoint,
	commitSig *btcec.Signature, revokeKey *btcec.PublicKey,
	obsfucator [6]byte) *SingleFundingComplete {

	return &SingleFundingComplete{
		PendingChannelID:    pChanID,
		FundingOutPoint:     fundingPoint,
		CommitSignature:     commitSig,
		RevocationKey:       revokeKey,
		StateHintObsfucator: obsfucator,
	}
}

// Decode deserializes the serialized SingleFundingComplete stored in the passed
// io.Reader into the target SingleFundingComplete using the deserialization
// rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		s.PendingChannelID[:],
		&s.FundingOutPoint,
		&s.CommitSignature,
		&s.RevocationKey,
		s.StateHintObsfucator[:])
}

// Encode serializes the target SingleFundingComplete into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		s.PendingChannelID[:],
		s.FundingOutPoint,
		s.CommitSignature,
		s.RevocationKey,
		s.StateHintObsfucator[:])
}

// Command returns the uint32 code which uniquely identifies this message as a
// SingleFundingComplete on the wire.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) Command() uint32 {
	return CmdSingleFundingComplete
}

// MaxPayloadLength returns the maximum allowed payload length for a
// SingleFundingComplete. This is calculated by summing the max length of all
// the fields within a SingleFundingComplete.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) MaxPayloadLength(uint32) uint32 {
	// 32 + 36 + 64 + 33 + 6
	return 171
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

	if s.CommitSignature == nil {
		return fmt.Errorf("commitment signature must be non-nil")
	}

	// TODO(roasbeef): fin validation

	// We're good!
	return nil
}
