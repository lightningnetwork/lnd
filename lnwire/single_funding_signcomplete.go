package lnwire

import (
	"fmt"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"io"
)

//Both parties send this message and then it is activated
type FundingSignComplete struct {
	ChannelID uint64

	TxID *wire.ShaHash
}

func (c *FundingSignComplete) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// TxID (32)
	err := readElements(r,
		&c.ChannelID,
		&c.TxID)
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
		c.ChannelID,
		c.TxID)
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
	return 40
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *FundingSignComplete) Validate() error {
	// We're good!
	return nil
}

func (c *FundingSignComplete) String() string {
	return fmt.Sprintf("\n--- Begin FundingSignComplete ---\n") +
		fmt.Sprintf("ChannelID:\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("TxID\t\t%s\n", c.TxID.String()) +
		fmt.Sprintf("--- End FundingSignComplete ---\n")
}
