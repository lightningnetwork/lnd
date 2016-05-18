package lnwire

import (
	"fmt"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"io"
)

type FundingResponse struct {
	ChannelID        uint64
	RevocationHash   [20]byte
	Pubkey           *btcec.PublicKey
	CommitSig        *btcec.Signature // Requester's Commitment
	DeliveryPkScript PkScript         // *MUST* be either P2PKH or P2SH
	ChangePkScript   PkScript         // *MUST* be either P2PKH or P2SH
}

func (c *FundingResponse) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// Revocation Hash (20)
	// Pubkey (32)
	// CommitSig (73)
	// DeliveryPkScript (final delivery)
	// ChangePkScript (change for extra from inputs)
	err := readElements(r,
		&c.ChannelID,
		&c.RevocationHash,
		&c.Pubkey,
		&c.CommitSig,
		&c.DeliveryPkScript,
		&c.ChangePkScript)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new FundingResponse
func NewFundingResponse() *FundingResponse {
	return &FundingResponse{}
}

// Serializes the item from the FundingResponse struct
// Writes the data to w
func (c *FundingResponse) Encode(w io.Writer, pver uint32) error {
	// ChannelID (8)
	// Revocation Hash (20)
	// Pubkey (32)
	// CommitSig (73)
	// DeliveryPkScript (final delivery)
	// ChangePkScript (change for extra from inputs)
	err := writeElements(w,
		c.ChannelID,
		c.RevocationHash,
		c.Pubkey,
		c.CommitSig,
		c.DeliveryPkScript,
		c.ChangePkScript)
	if err != nil {
		return err
	}

	return nil
}

func (c *FundingResponse) Command() uint32 {
	return CmdFundingResponse
}

func (c *FundingResponse) MaxPayloadLength(uint32) uint32 {
	return 186
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *FundingResponse) Validate() error {
	var err error

	// No negative values
	if c.ResponderFundingAmount < 0 {
		return fmt.Errorf("ResponderFundingAmount cannot be negative")
	}

	if c.ResponderReserveAmount < 0 {
		return fmt.Errorf("ResponderReserveAmount cannot be negative")
	}

	if c.MinFeePerKb < 0 {
		return fmt.Errorf("MinFeePerKb cannot be negative")
	}

	// Validation of what makes sense...
	if c.ResponderFundingAmount < c.ResponderReserveAmount {
		return fmt.Errorf("Reserve must be below Funding Amount")
	}

	// Make sure there's not more than 127 inputs
	if len(c.Inputs) > 127 {
		return fmt.Errorf("Too many inputs")
	}

	// Delivery PkScript is either P2SH or P2PKH
	err = ValidatePkScript(c.DeliveryPkScript)
	if err != nil {
		return err
	}

	// Change PkScript is either P2SH or P2PKH
	err = ValidatePkScript(c.ChangePkScript)
	if err != nil {
		return err
	}

	// We're good!
	return nil
}

func (c *FundingResponse) String() string {
	var serializedPubkey []byte
	if &c.Pubkey != nil && c.Pubkey.X != nil {
		serializedPubkey = c.Pubkey.SerializeCompressed()
	}

	return fmt.Sprintf("\n--- Begin FundingResponse ---\n") +
		fmt.Sprintf("ChannelID:\t\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("RevocationHash\t\t\t%x\n", c.RevocationHash) +
		fmt.Sprintf("Pubkey\t\t\t\t%x\n", serializedPubkey) +
		fmt.Sprintf("CommitSig\t\t\t%x\n", c.CommitSig.Serialize()) +
		fmt.Sprintf("DeliveryPkScript\t\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("ChangePkScript\t\t%x\n", c.ChangePkScript) +
		fmt.Sprintf("--- End FundingResponse ---\n")
}
