package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

type FundingRequest struct {
	ReservationID uint64

	ChannelType uint8

	RequesterFundingAmount btcutil.Amount
	RequesterReserveAmount btcutil.Amount
	MinFeePerKb            btcutil.Amount

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

	// Minimum number of confirmations to validate transaction
	MinDepth uint32

	// Should double-check the total funding later
	MinTotalFundingAmount btcutil.Amount

	// CLTV/CSV lock-time to use
	LockTime uint32

	// Who pays the fees
	// 0: (default) channel initiator
	// 1: split
	// 2: channel responder
	FeePayer uint8

	RevocationHash   [20]byte
	Pubkey           *btcec.PublicKey
	DeliveryPkScript PkScript // *MUST* be either P2PKH or P2SH
	ChangePkScript   PkScript // *MUST* be either P2PKH or P2SH

	Inputs []*wire.TxIn
}

func (c *FundingRequest) Decode(r io.Reader, pver uint32) error {
	// Reservation ID (8)
	// Channel Type (1)
	// Funding Amount (8)
	// Channel Minimum Capacity (8)
	// Revocation Hash (20)
	// Commitment Pubkey (32)
	// Reserve Amount (8)
	// Minimum Transaction Fee Per Kb (8)
	// PaymentAmount (8)
	// MinDepth (4)
	// LockTime (4)
	// FeePayer (1)
	// DeliveryPkScript (final delivery)
	// 	First byte length then pkscript
	// ChangePkScript (change for extra from inputs)
	// 	First byte length then pkscript
	// Inputs: Create the TxIns
	// 	First byte is number of inputs
	// 	For each input, it's 32bytes txin & 4bytes index
	err := readElements(r,
		&c.ReservationID,
		&c.ChannelType,
		&c.RequesterFundingAmount,
		&c.MinTotalFundingAmount,
		&c.RevocationHash,
		&c.Pubkey,
		&c.RequesterReserveAmount,
		&c.MinFeePerKb,
		&c.PaymentAmount,
		&c.MinDepth,
		&c.LockTime,
		&c.FeePayer,
		&c.DeliveryPkScript,
		&c.ChangePkScript,
		&c.Inputs)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new FundingRequest
func NewFundingRequest() *FundingRequest {
	return &FundingRequest{}
}

// Serializes the item from the FundingRequest struct
// Writes the data to w
func (c *FundingRequest) Encode(w io.Writer, pver uint32) error {
	// Channel Type
	// Funding Amont
	// Channel Minimum Capacity
	// Revocation Hash
	// Commitment Pubkey
	// Reserve Amount
	// Minimum Transaction Fee Per KB
	// LockTime
	// FeePayer
	// DeliveryPkScript
	// ChangePkScript
	// Inputs: Append the actual Txins
	err := writeElements(w,
		c.ReservationID,
		c.ChannelType,
		c.RequesterFundingAmount,
		c.MinTotalFundingAmount,
		c.RevocationHash,
		c.Pubkey,
		c.RequesterReserveAmount,
		c.MinFeePerKb,
		c.PaymentAmount,
		c.MinDepth,
		c.LockTime,
		c.FeePayer,
		c.DeliveryPkScript,
		c.ChangePkScript,
		c.Inputs)
	if err != nil {
		return err
	}

	return nil
}

func (c *FundingRequest) Command() uint32 {
	return CmdFundingRequest
}

func (c *FundingRequest) MaxPayloadLength(uint32) uint32 {
	// 110 (base size) + 26 (pkscript) + 26 (pkscript) + 1 (numTxes) + 127*36(127 inputs * sha256+idx)
	return 4735
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *FundingRequest) Validate() error {
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
	if !isValidPkScript(c.DeliveryPkScript) {
		// TODO(roasbeef): move into actual error
		return fmt.Errorf("Valid delivery public key scripts MUST be: " +
			"P2PKH, P2WKH, P2SH, or P2WSH.")
	}

	// ChangePkScript is either P2SH or P2PKH
	if !isValidPkScript(c.ChangePkScript) {
		return fmt.Errorf("Valid change public key script MUST be: " +
			"P2PKH, P2WKH, P2SH, or P2WSH.")
	}

	// We're good!
	return nil
}

func (c *FundingRequest) String() string {
	var inputs string
	for i, in := range c.Inputs {
		inputs += fmt.Sprintf("\n     Slice\t%d\n", i)
		if &in != nil {
			inputs += fmt.Sprintf("\tHash\t%s\n", in.PreviousOutPoint.Hash)
			inputs += fmt.Sprintf("\tIndex\t%d\n", in.PreviousOutPoint.Index)
		}
	}

	var serializedPubkey []byte
	if &c.Pubkey != nil && c.Pubkey.X != nil {
		serializedPubkey = c.Pubkey.SerializeCompressed()
	}

	return fmt.Sprintf("\n--- Begin FundingRequest ---\n") +
		fmt.Sprintf("ReservationID:\t\t\t%d\n", c.ReservationID) +
		fmt.Sprintf("ChannelType:\t\t\t%x\n", c.ChannelType) +
		fmt.Sprintf("RequesterFundingAmount:\t\t%s\n", c.RequesterFundingAmount.String()) +
		fmt.Sprintf("RequesterReserveAmount:\t\t%s\n", c.RequesterReserveAmount.String()) +
		fmt.Sprintf("MinFeePerKb:\t\t\t%s\n", c.MinFeePerKb.String()) +
		fmt.Sprintf("PaymentAmount:\t\t\t%s\n", c.PaymentAmount.String()) +
		fmt.Sprintf("MinDepth:\t\t\t%d\n", c.MinDepth) +
		fmt.Sprintf("MinTotalFundingAmount\t\t%s\n", c.MinTotalFundingAmount.String()) +
		fmt.Sprintf("LockTime\t\t\t%d\n", c.LockTime) +
		fmt.Sprintf("FeePayer\t\t\t%x\n", c.FeePayer) +
		fmt.Sprintf("RevocationHash\t\t\t%x\n", c.RevocationHash) +
		fmt.Sprintf("Pubkey\t\t\t\t%x\n", serializedPubkey) +
		fmt.Sprintf("DeliveryPkScript\t\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("Inputs:") +
		inputs +
		fmt.Sprintf("--- End FundingRequest ---\n")
}
