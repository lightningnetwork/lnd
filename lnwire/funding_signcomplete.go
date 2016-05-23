package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
)

type FundingSignComplete struct {
	ReservationID uint64

	TxID          *wire.ShaHash
	FundingTXSigs []*btcec.Signature
}

func (c *FundingSignComplete) Decode(r io.Reader, pver uint32) error {
	// ReservationID (8)
	// TxID (32)
	// FundingTXSigs
	// 	First byte is number of FundingTxSigs
	// 	Sorted list of the requester's input signatures
	// 	(originally provided in the Funding Request)
	err := readElements(r,
		&c.ReservationID,
		&c.TxID,
		&c.FundingTXSigs)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new FundingSignComplete
func NewFundingSignComplete() *FundingSignComplete {
	return &FundingSignComplete{}
}

// Serializes the item from the FundingSignComplete struct
// Writes the data to w
func (c *FundingSignComplete) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ReservationID,
		c.TxID,
		c.FundingTXSigs)
	if err != nil {
		return err
	}

	return nil
}

func (c *FundingSignComplete) Command() uint32 {
	return CmdFundingSignComplete
}

func (c *FundingSignComplete) MaxPayloadLength(uint32) uint32 {
	// 8 (base size) + 32 + (73maxSigSize*127maxInputs)
	return 9311
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *FundingSignComplete) Validate() error {
	// We're good!
	return nil
}

func (c *FundingSignComplete) String() string {
	var sigs string
	for i, in := range c.FundingTXSigs {
		sigs += fmt.Sprintf("\n     Slice\t%d\n", i)
		if in != nil {
			sigs += fmt.Sprintf("\tSig\t%x\n", in.Serialize())
		}
	}

	return fmt.Sprintf("\n--- Begin FundingSignComplete ---\n") +
		fmt.Sprintf("ReservationID:\t\t%d\n", c.ReservationID) +
		fmt.Sprintf("TxID\t\t%s\n", c.TxID.String()) +
		fmt.Sprintf("FundingTxSigs:") +
		sigs +
		fmt.Sprintf("--- End FundingSignComplete ---\n")
}
