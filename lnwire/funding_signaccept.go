package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
)

type FundingSignAccept struct {
	ReservationID uint64

	CommitSig     *btcec.Signature // Requester's Commitment
	FundingTXSigs []*btcec.Signature
}

func (c *FundingSignAccept) Decode(r io.Reader, pver uint32) error {
	// ReservationID (8)
	// CommitSig (73)
	// 	First byte length then sig
	// FundingTXSigs
	// 	First byte is number of FundingTxSigs
	// 	Sorted list of the requester's input signatures
	// 	(originally provided in the Funding Request)
	err := readElements(r,
		&c.ReservationID,
		&c.CommitSig,
		&c.FundingTXSigs)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new FundingSignAccept
func NewFundingSignAccept() *FundingSignAccept {
	return &FundingSignAccept{}
}

// Serializes the item from the FundingSignAccept struct
// Writes the data to w
func (c *FundingSignAccept) Encode(w io.Writer, pver uint32) error {
	// ReservationID
	// CommitSig
	// FundingTxSigs
	err := writeElements(w,
		c.ReservationID,
		c.CommitSig,
		c.FundingTXSigs)
	if err != nil {
		return err
	}

	return nil
}

func (c *FundingSignAccept) Command() uint32 {
	return CmdFundingSignAccept
}

func (c *FundingSignAccept) MaxPayloadLength(uint32) uint32 {
	// 8 (base size) + 73 + (73maxSigSize*127maxInputs)
	return 9352
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *FundingSignAccept) Validate() error {
	// We're good!
	return nil
}

func (c *FundingSignAccept) String() string {
	var sigs string
	for i, in := range c.FundingTXSigs {
		sigs += fmt.Sprintf("\n     Slice\t%d\n", i)
		if in != nil {
			sigs += fmt.Sprintf("\tSig\t%x\n", in.Serialize())
		}
	}

	var serializedSig []byte
	if &c.CommitSig != nil && c.CommitSig.R != nil {
		serializedSig = c.CommitSig.Serialize()
	}

	return fmt.Sprintf("\n--- Begin FundingSignAccept ---\n") +
		fmt.Sprintf("ReservationID:\t\t%d\n", c.ReservationID) +
		fmt.Sprintf("CommitSig\t\t%x\n", serializedSig) +
		fmt.Sprintf("FundingTxSigs:") +
		sigs +
		fmt.Sprintf("--- End FundingSignAccept ---\n")
}
