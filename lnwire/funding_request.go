package lnwire

import (
	"bytes"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"io"
)

type FundingRequest struct {
	ChannelType uint8

	FundingAmount btcutil.Amount
	ReserveAmount btcutil.Amount
	MinFeePerKb   btcutil.Amount

	//Should double-check the total funding later
	MinTotalFundingAmount btcutil.Amount

	//CLTV lock-time to use
	LockTime uint32

	//Who pays the fees
	//0: (default) channel initiator
	//1: split
	//2: channel responder
	FeePayer uint8

	RevocationHash   [20]byte
	Pubkey           *btcec.PublicKey
	DeliveryPkScript PkScript //*MUST* be either P2PKH or P2SH

	Inputs []*wire.TxIn
}

func (c *FundingRequest) Decode(r io.Reader, pver uint32) error {
	//Channel Type (0/1)
	//	default to 0 for CLTV-only
	//Funding Amount (1/8)
	//Channel Minimum Capacity (9/8)
	//Revocation Hash (17/20)
	//Commitment Pubkey (37/32)
	//Reserve Amount (69/8)
	//Minimum Transaction Fee Per Kb (77/8)
	//LockTime (85/4)
	//FeePayer (89/1)
	//DeliveryPkScript
	//	First byte length then pkscript
	//Inputs: Create the TxIns
	//	First byte is number of inputs
	//	For each input, it's 32bytes txin & 4bytes index
	err := readElements(r, false,
		&c.ChannelType,
		&c.FundingAmount,
		&c.MinTotalFundingAmount,
		&c.RevocationHash,
		&c.Pubkey,
		&c.ReserveAmount,
		&c.MinFeePerKb,
		&c.LockTime,
		&c.FeePayer,
		&c.DeliveryPkScript,
		&c.Inputs)
	if err != nil {
		return err
	}

	return c.Validate()
}

//Creates a new FundingRequest
func NewFundingRequest() *FundingRequest {
	return &FundingRequest{}
}

//Serializes the item from the FundingRequest struct
//Writes the data to w
func (c *FundingRequest) Encode(w io.Writer, pver uint32) error {
	//Channel Type
	//Funding Amont
	//Channel Minimum Capacity
	//Revocation Hash
	//Commitment Pubkey
	//Reserve Amount
	//Minimum Transaction Fee Per KB
	//LockTime
	//FeePayer
	//DeliveryPkScript
	//Inputs: Append the actual Txins
	err := writeElements(w, false,
		c.ChannelType,
		c.FundingAmount,
		c.MinTotalFundingAmount,
		c.RevocationHash,
		c.Pubkey,
		c.ReserveAmount,
		c.MinFeePerKb,
		c.LockTime,
		c.FeePayer,
		c.DeliveryPkScript,
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
	//90 (base size) + 26 (pkscript) + 1 (numTxes) + 127*36(127 inputs * sha256+idx)
	//4690
	return 4689
}

//Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *FundingRequest) Validate() error {
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
	if c.MinTotalFundingAmount < 0 {
		return fmt.Errorf("MinTotalFundingAmount cannot be negative")
	}

	//PkScript is either P2SH or P2PKH
	//P2PKH
	if len(c.DeliveryPkScript) == 25 {
		//Begins with OP_DUP OP_HASH160 PUSHDATA(20)
		if !(bytes.Equal(c.DeliveryPkScript[0:3], []byte{118, 169, 20}) &&
			//Ends with OP_EQUALVERIFY OP_CHECKSIG
			bytes.Equal(c.DeliveryPkScript[23:25], []byte{136, 172})) {
			//If it's not correct, return error
			return fmt.Errorf("PkScript only allows P2SH or P2PKH")
		}
		//P2SH
	} else if len(c.DeliveryPkScript) == 23 {
		//Begins with OP_HASH160 PUSHDATA(20)
		if !(bytes.Equal(c.DeliveryPkScript[0:2], []byte{169, 20}) &&
			//Ends with OP_EQUAL
			bytes.Equal(c.DeliveryPkScript[22:23], []byte{135})) {
			//If it's not correct, return error
			return fmt.Errorf("PkScript only allows P2SH or P2PKH")
		}
	} else {
		//Length not 23 or 25
		return fmt.Errorf("PkScript only allows P2SH or P2PKH")
	}

	//We're good!
	return nil
}

func (c *FundingRequest) String() string {
	var inputs string
	for i, in := range c.Inputs {
		inputs += fmt.Sprintf("\n     Slice\t%d\n", i)
		inputs += fmt.Sprintf("\tHash\t%s\n", in.PreviousOutPoint.Hash)
		inputs += fmt.Sprintf("\tIndex\t%d\n", in.PreviousOutPoint.Index)
	}
	return fmt.Sprintf("\n--- Begin FundingRequest ---\n") +
		fmt.Sprintf("ChannelType:\t\t%x\n", c.ChannelType) +
		fmt.Sprintf("FundingAmount:\t\t%s\n", c.FundingAmount.String()) +
		fmt.Sprintf("ReserveAmount:\t\t%s\n", c.ReserveAmount.String()) +
		fmt.Sprintf("MinFeePerKb:\t\t%s\n", c.MinFeePerKb.String()) +
		fmt.Sprintf("MinTotalFundingAmount\t%s\n", c.MinTotalFundingAmount.String()) +
		fmt.Sprintf("LockTime\t\t%d\n", c.LockTime) +
		fmt.Sprintf("FeePayer\t\t%x\n", c.FeePayer) +
		fmt.Sprintf("RevocationHash\t\t%x\n", c.RevocationHash) +
		fmt.Sprintf("Pubkey\t\t\t%x\n", c.Pubkey.SerializeCompressed()) +
		fmt.Sprintf("DeliveryPkScript\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("Inputs:") +
		inputs +
		fmt.Sprintf("--- End FundingRequest ---\n")
}
