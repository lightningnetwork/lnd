package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

type FundingResponse struct {
	ChannelType uint8

	ReservationID uint64

	ResponderFundingAmount btcutil.Amount // Responder's funding amount
	ResponderReserveAmount btcutil.Amount // Responder's reserve amount
	MinFeePerKb            btcutil.Amount // Lock-in min fee

	// Minimum depth
	MinDepth uint32

	// CLTV/CSV lock-time to use
	LockTime uint32

	// Who pays the fees
	// 0: (default) channel initiator
	// 1: split
	// 2: channel responder
	FeePayer uint8

	RevocationHash   [20]byte
	Pubkey           *btcec.PublicKey
	CommitSig        *btcec.Signature // Requester's Commitment
	DeliveryPkScript PkScript         // *MUST* be either P2PKH or P2SH
	ChangePkScript   PkScript         // *MUST* be either P2PKH or P2SH

	Inputs []*wire.TxIn
}

func (c *FundingResponse) Decode(r io.Reader, pver uint32) error {
	// ReservationID (8)
	// Channel Type (1)
	// Funding Amount (8)
	// Revocation Hash (20)
	// Commitment Pubkey (32)
	// Reserve Amount (8)
	// Minimum Transaction Fee Per Kb (8)
	// MinDepth (4)
	// LockTime (4)
	// FeePayer (1)
	// DeliveryPkScript (final delivery)
	// 	First byte length then pkscript
	// ChangePkScript (change for extra from inputs)
	// 	First byte length then pkscript
	// CommitSig
	// 	First byte length then sig
	// Inputs: Create the TxIns
	// 	First byte is number of inputs
	// 	For each input, it's 32bytes txin & 4bytes index
	err := readElements(r,
		&c.ReservationID,
		&c.ChannelType,
		&c.ResponderFundingAmount,
		&c.RevocationHash,
		&c.Pubkey,
		&c.ResponderReserveAmount,
		&c.MinFeePerKb,
		&c.MinDepth,
		&c.LockTime,
		&c.FeePayer,
		&c.DeliveryPkScript,
		&c.ChangePkScript,
		&c.CommitSig,
		&c.Inputs)
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
	// ReservationID (8)
	// Channel Type (1)
	// Funding Amount (8)
	// Revocation Hash (20)
	// Commitment Pubkey (32)
	// Reserve Amount (8)
	// Minimum Transaction Fee Per Kb (8)
	// LockTime (4)
	// FeePayer (1)
	// DeliveryPkScript (final delivery)
	// ChangePkScript (change for extra from inputs)
	// CommitSig
	// Inputs
	err := writeElements(w,
		c.ReservationID,
		c.ChannelType,
		c.ResponderFundingAmount,
		c.RevocationHash,
		c.Pubkey,
		c.ResponderReserveAmount,
		c.MinFeePerKb,
		c.MinDepth,
		c.LockTime,
		c.FeePayer,
		c.DeliveryPkScript,
		c.ChangePkScript,
		c.CommitSig,
		c.Inputs)
	if err != nil {
		return err
	}

	return nil
}

func (c *FundingResponse) Command() uint32 {
	return CmdFundingResponse
}

func (c *FundingResponse) MaxPayloadLength(uint32) uint32 {
	// 86 (base size) + 26 (pkscript) + 26 (pkscript) + 74sig + 1 (numTxes) + 127*36(127 inputs * sha256+idx)
	return 4785
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *FundingResponse) Validate() error {
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
	if !isValidPkScript(c.DeliveryPkScript) {
		return fmt.Errorf("Valid delivery public key scripts MUST be: " +
			"P2PKH, P2WKH, P2SH, or P2WSH.")
	}

	// Change PkScript is either P2SH or P2PKH
	if !isValidPkScript(c.ChangePkScript) {
		// TODO(roasbeef): move into actual error
		return fmt.Errorf("Valid change public key scripts MUST be: " +
			"P2PKH, P2WKH, P2SH, or P2WSH.")
	}

	// We're good!
	return nil
}

func (c *FundingResponse) String() string {
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

	return fmt.Sprintf("\n--- Begin FundingResponse ---\n") +
		fmt.Sprintf("ChannelType:\t\t\t%x\n", c.ChannelType) +
		fmt.Sprintf("ReservationID:\t\t\t%d\n", c.ReservationID) +
		fmt.Sprintf("ResponderFundingAmount:\t\t%s\n", c.ResponderFundingAmount.String()) +
		fmt.Sprintf("ResponderReserveAmount:\t\t%s\n", c.ResponderReserveAmount.String()) +
		fmt.Sprintf("MinFeePerKb:\t\t\t%s\n", c.MinFeePerKb.String()) +
		fmt.Sprintf("MinDepth:\t\t\t%d\n", c.MinDepth) +
		fmt.Sprintf("LockTime\t\t\t%d\n", c.LockTime) +
		fmt.Sprintf("FeePayer\t\t\t%x\n", c.FeePayer) +
		fmt.Sprintf("RevocationHash\t\t\t%x\n", c.RevocationHash) +
		fmt.Sprintf("Pubkey\t\t\t\t%x\n", serializedPubkey) +
		fmt.Sprintf("CommitSig\t\t\t%x\n", c.CommitSig.Serialize()) +
		fmt.Sprintf("DeliveryPkScript\t\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("ChangePkScript\t\t%x\n", c.ChangePkScript) +
		fmt.Sprintf("Inputs:") +
		inputs +
		fmt.Sprintf("--- End FundingResponse ---\n")
}
