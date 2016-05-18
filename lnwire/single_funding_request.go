package lnwire

import (
	"fmt"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"io"
)

type SingleFundingRequest struct {
	//ChannelID
	ChannelID uint64

	//Default 0
	ChannelType uint8

	//Bitcoin: 0.
	CoinType uint64

	//Amount of fees per kb
	//Assumes requester pays
	FeePerKb btcutil.Amount

	// The funding requester can request payment
	// This wallet only allows positive values,
	// which is a payment to the responder
	// (This can be used to fund the Reserve)
	// If the responder disagrees, then the funding request fails
	// THIS VALUE GOES INTO THE RESPONDER'S FUNDING AMOUNT
	// total requester input value = RequesterFundingAmount + PaymentAmount + "Total Change" + Fees(?)
	// RequesterFundingAmount = "Available Balance" + RequesterReserveAmount
	// Payment SHOULD NOT be acknowledged until the minimum confirmation has elapsed
	// (Due to double-spend risks the recipient will not want to acknolwedge confirmation until later)
	// This is to make a payment as part of opening the channel
	PaymentAmount btcutil.Amount

	// CLTV/CSV lock-time to use
	LockTime uint32

	RevocationHash   [20]byte
	Pubkey           *btcec.PublicKey
	DeliveryPkScript PkScript // *MUST* be either P2PKH or P2SH
	ChangePkScript   PkScript // *MUST* be either P2PKH or P2SH
}

func (c *SingleFundingRequest) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// ChannelType (1)
	// CoinType	(8)
	// FeePerKb (8)
	// PaymentAmount (8)
	// LockTime (4)
	// Revocation Hash (20)
	// Pubkey (32)
	// DeliveryPkScript (final delivery)
	// ChangePkScript (change for extra from inputs)
	err := readElements(r,
		&c.ChannelID,
		&c.ChannelType,
		&c.CoinType,
		&c.FeePerKb,
		&c.PaymentAmount,
		&c.LockTime,
		&c.RevocationHash,
		&c.Pubkey,
		&c.DeliveryPkScript,
		&c.ChangePkScript)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new SingleFundingRequest
func NewSingleFundingRequest() *SingleFundingRequest {
	return &SingleFundingRequest{}
}

// Serializes the item from the SingleFundingRequest struct
// Writes the data to w
func (c *SingleFundingRequest) Encode(w io.Writer, pver uint32) error {
	// ChannelID (8)
	// ChannelType (1)
	// CoinType	(8)
	// FeePerKb (8)
	// PaymentAmount (8)
	// LockTime (4)
	// Revocation Hash (20)
	// Pubkey (32)
	// DeliveryPkScript (final delivery)
	// ChangePkScript (change for extra from inputs)
	err := writeElements(w,
		c.ChannelID,
		c.ChannelType,
		c.CoinType,
		c.FeePerKb,
		c.PaymentAmount,
		c.LockTime,
		c.RevocationHash,
		c.Pubkey,
		c.DeliveryPkScript,
		c.ChangePkScript)
	if err != nil {
		return err
	}

	return nil
}

func (c *SingleFundingRequest) Command() uint32 {
	return CmdSingleFundingRequest
}

func (c *SingleFundingRequest) MaxPayloadLength(uint32) uint32 {
	return 141
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *SingleFundingRequest) Validate() error {
	var err error

	// No negative values
	if c.RequesterFundingAmount < 0 {
		return fmt.Errorf("RequesterFundingAmount cannot be negative")
	}

	if c.RequesterReserveAmount < 0 {
		return fmt.Errorf("RequesterReserveAmount cannot be negative")
	}

	if c.MinFeePerKb < 0 {
		return fmt.Errorf("MinFeePerKb cannot be negative")
	}
	if c.MinTotalFundingAmount < 0 {
		return fmt.Errorf("MinTotalFundingAmount cannot be negative")
	}

	// Validation of what makes sense...
	if c.MinTotalFundingAmount < c.RequesterFundingAmount {
		return fmt.Errorf("Requester's minimum too low.")
	}
	if c.RequesterFundingAmount < c.RequesterReserveAmount {
		return fmt.Errorf("Reserve must be below Funding Amount")
	}

	// This wallet only allows payment from the requester to responder
	if c.PaymentAmount < 0 {
		return fmt.Errorf("This wallet requieres payment to be greater than zero.")
	}

	// Make sure there's not more than 127 inputs
	if len(c.Inputs) > 127 {
		return fmt.Errorf("Too many inputs")
	}

	// DeliveryPkScript is either P2SH or P2PKH
	err = ValidatePkScript(c.DeliveryPkScript)
	if err != nil {
		return err
	}

	// ChangePkScript is either P2SH or P2PKH
	err = ValidatePkScript(c.ChangePkScript)
	if err != nil {
		return err
	}

	// We're good!
	return nil
}

func (c *SingleFundingRequest) String() string {
	var serializedPubkey []byte
	if &c.Pubkey != nil && c.Pubkey.X != nil {
		serializedPubkey = c.Pubkey.SerializeCompressed()
	}

	return fmt.Sprintf("\n--- Begin SingleFundingRequest ---\n") +
		fmt.Sprintf("ChannelID:\t\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("ChannelType:\t\t\t%x\n", c.ChannelType) +
		fmt.Sprintf("CoinType:\t\t\t%d\n", c.CoinType) +
		fmt.Sprintf("FeePerKb:\t\t\t%s\n", c.FeePerKb.String()) +
		fmt.Sprintf("PaymentAmount:\t\t\t%s\n", c.PaymentAmount.String()) +
		fmt.Sprintf("LockTime\t\t\t%d\n", c.LockTime) +
		fmt.Sprintf("RevocationHash\t\t\t%x\n", c.RevocationHash) +
		fmt.Sprintf("Pubkey\t\t\t\t%x\n", serializedPubkey) +
		fmt.Sprintf("DeliveryPkScript\t\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("ChangePkScript\t\t%x\n", c.ChangePkScript) +
		fmt.Sprintf("--- End SingleFundingRequest ---\n")
}
