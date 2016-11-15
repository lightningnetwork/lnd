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
	// TODO(roasbeef): change all to PendingChannelID, document schema
	ChannelID uint64

	// FundingOutPoint is the outpoint (txid:index) of the funding
	// transaction. With this value, Bob will be able to generate a
	// signature for Alice's version of the commitment transaction.
	FundingOutPoint *wire.OutPoint

	// CommitSignature is Alice's signature for Bob's version of the
	// commitment transaction.
	CommitSignature *btcec.Signature

	// RevocationKey is the initial key to be used for the revocation
	// clause within the self-output of the initiators's commitment
	// transaction. Once an initial new state is created, the initiator
	// will send a pre-image which will allow the initiator to sweep the
	// initiator's funds if the violate the contract.
	RevocationKey *btcec.PublicKey

	// StateHintObsfucator is the set of bytes used by the initiator to
	// obsfucate the state number encoded within the sequence number for
	// the commitment transaction's only input. The initiator generates
	// this value by hashing the 0th' state derived from the revocation PRF
	// an additional time.
	StateHintObsfucator [4]byte
}

// NewSingleFundingComplete creates, and returns a new empty
// SingleFundingResponse.
func NewSingleFundingComplete(chanID uint64, fundingPoint *wire.OutPoint,
	commitSig *btcec.Signature, revokeKey *btcec.PublicKey,
	obsfucator [4]byte) *SingleFundingComplete {

	return &SingleFundingComplete{
		ChannelID:           chanID,
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
	// ChannelID (8)
	// FundingOutPoint (36)
	// CommitmentSignature (73)
	// RevocationKey (33)
	err := readElements(r,
		&s.ChannelID,
		&s.FundingOutPoint,
		&s.CommitSignature,
		&s.RevocationKey,
		&s.StateHintObsfucator)
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
	// RevocationKey (33)
	err := writeElements(w,
		s.ChannelID,
		s.FundingOutPoint,
		s.CommitSignature,
		s.RevocationKey,
		s.StateHintObsfucator)
	if err != nil {
		return err
	}

	return nil
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
// the fields within a SingleFundingResponse. Therefore, the final breakdown
// is: 8 + 36 + 33 + 73 + 4 = 154
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) MaxPayloadLength(uint32) uint32 {
	return 154
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

// String returns the string representation of the SingleFundingResponse.
//
// This is part of the lnwire.Message interface.
func (s *SingleFundingComplete) String() string {
	var rk []byte
	if s.RevocationKey != nil {
		rk = s.RevocationKey.SerializeCompressed()
	}

	return fmt.Sprintf("\n--- Begin SingleFundingComplete ---\n") +
		fmt.Sprintf("ChannelID:\t\t\t%d\n", s.ChannelID) +
		fmt.Sprintf("FundingOutPoint:\t\t\t%x\n", s.FundingOutPoint) +
		fmt.Sprintf("CommitSignature\t\t\t\t%x\n", s.CommitSignature) +
		fmt.Sprintf("RevocationKey\t\t\t\t%x\n", rk) +
		fmt.Sprintf("StateHintObsfucator\t\t\t%x\n", s.StateHintObsfucator) +
		fmt.Sprintf("--- End SingleFundingComplete ---\n")
}
