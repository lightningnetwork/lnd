package lnwire

import (
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"io"
)

type FundingResponse struct {
	ChannelType uint8

	ReservationID uint64

	FundingAmount btcutil.Amount
	ReserveAmount btcutil.Amount
	MinFeePerKb   btcutil.Amount //Lock-in min fee

	//CLTV/CSV lock-time to use
	LockTime uint32

	//Who pays the fees
	//0: (default) channel initiator
	//1: split
	//2: channel responder
	FeePayer uint8

	RevocationHash   [20]byte
	Pubkey           *btcec.PublicKey
	CommitSig        *btcec.Signature //Requester's Commitment
	DeliveryPkScript PkScript         //*MUST* be either P2PKH or P2SH
	ChangePkScript   PkScript         //*MUST* be either P2PKH or P2SH

	Inputs []*wire.TxIn
}

func (c *FundingResponse) Decode(r io.Reader, pver uint32) error {
	//ReservationID (0/8)
	//Channel Type (8/1)
	//Funding Amount (9/8)
	//Revocation Hash (29/20)
	//Commitment Pubkey (61/32)
	//Reserve Amount (69/8)
	//Minimum Transaction Fee Per Kb (77/8)
	//LockTime (81/4)
	//FeePayer (82/1)
	//DeliveryPkScript (final delivery)
	//	First byte length then pkscript
	//ChangePkScript (change for extra from inputs)
	//	First byte length then pkscript
	//CommitSig
	//	First byte length then sig
	//Inputs: Create the TxIns
	//	First byte is number of inputs
	//	For each input, it's 32bytes txin & 4bytes index
	err := readElements(r, false,
		&c.ReservationID,
		&c.ChannelType,
		&c.FundingAmount,
		&c.RevocationHash,
		&c.Pubkey,
		&c.ReserveAmount,
		&c.MinFeePerKb,
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

//Creates a new FundingResponse
func NewFundingResponse() *FundingResponse {
	return &FundingResponse{}
}

//Serializes the item from the FundingResponse struct
//Writes the data to w
func (c *FundingResponse) Encode(w io.Writer, pver uint32) error {
	//ReservationID (8)
	//Channel Type (1)
	//Funding Amount (8)
	//Revocation Hash (20)
	//Commitment Pubkey (32)
	//Reserve Amount (8)
	//Minimum Transaction Fee Per Kb (8)
	//LockTime (4)
	//FeePayer (1)
	//DeliveryPkScript (final delivery)
	//ChangePkScript (change for extra from inputs)
	//CommitSig
	//Inputs
	err := writeElements(w, false,
		c.ReservationID,
		c.ChannelType,
		c.FundingAmount,
		c.RevocationHash,
		c.Pubkey,
		c.ReserveAmount,
		c.MinFeePerKb,
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
	//82 (base size) + 26 (pkscript) + 26 (pkscript) + 74sig + 1 (numTxes) + 127*36(127 inputs * sha256+idx)
	return 4781
}

//Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *FundingResponse) Validate() error {
	var err error

	//No negative values
	if c.FundingAmount < 0 {
		return fmt.Errorf("FundingAmount cannot be negative")
	}

	if c.ReserveAmount < 0 {
		return fmt.Errorf("ReserveAmount cannot be negative")
	}

	if c.MinFeePerKb < 0 {
		return fmt.Errorf("MinFeePerKb cannot be negative")
	}

	//Delivery PkScript is either P2SH or P2PKH
	err = ValidatePkScript(c.DeliveryPkScript)
	if err != nil {
		return err
	}

	//Change PkScript is either P2SH or P2PKH
	err = ValidatePkScript(c.ChangePkScript)
	if err != nil {
		return err
	}

	//We're good!
	return nil
}

func (c *FundingResponse) String() string {
	var inputs string
	for i, in := range c.Inputs {
		inputs += fmt.Sprintf("\n     Slice\t%d\n", i)
		inputs += fmt.Sprintf("\tHash\t%s\n", in.PreviousOutPoint.Hash)
		inputs += fmt.Sprintf("\tIndex\t%d\n", in.PreviousOutPoint.Index)
	}
	return fmt.Sprintf("\n--- Begin FundingResponse ---\n") +
		fmt.Sprintf("ChannelType:\t\t%x\n", c.ChannelType) +
		fmt.Sprintf("ReservationID:\t\t%d\n", c.ReservationID) +
		fmt.Sprintf("FundingAmount:\t\t%s\n", c.FundingAmount.String()) +
		fmt.Sprintf("ReserveAmount:\t\t%s\n", c.ReserveAmount.String()) +
		fmt.Sprintf("MinFeePerKb:\t\t%s\n", c.MinFeePerKb.String()) +
		fmt.Sprintf("LockTime\t\t%d\n", c.LockTime) +
		fmt.Sprintf("FeePayer\t\t%x\n", c.FeePayer) +
		fmt.Sprintf("RevocationHash\t\t%x\n", c.RevocationHash) +
		fmt.Sprintf("Pubkey\t\t\t%x\n", c.Pubkey.SerializeCompressed()) +
		fmt.Sprintf("CommitSig\t\t%x\n", c.CommitSig.Serialize()) +
		fmt.Sprintf("DeliveryPkScript\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("ChangePkScript\t%x\n", c.ChangePkScript) +
		fmt.Sprintf("Inputs:") +
		inputs +
		fmt.Sprintf("--- End FundingResponse ---\n")
}
