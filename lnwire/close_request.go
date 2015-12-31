package lnwire

import (
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"

	"io"
)

type CloseRequest struct {
	ReservationID uint64

	RequesterCloseSig *btcec.Signature //Requester's Commitment
	Fee               btcutil.Amount
}

func (c *CloseRequest) Decode(r io.Reader, pver uint32) error {
	//ReservationID (8)
	//RequesterCloseSig (73)
	//	First byte length then sig
	//Fee (8)
	err := readElements(r,
		&c.ReservationID,
		&c.RequesterCloseSig,
		&c.Fee)
	if err != nil {
		return err
	}

	return nil
}

//Creates a new CloseRequest
func NewCloseRequest() *CloseRequest {
	return &CloseRequest{}
}

//Serializes the item from the CloseRequest struct
//Writes the data to w
func (c *CloseRequest) Encode(w io.Writer, pver uint32) error {
	//ReservationID
	//RequesterCloseSig
	//Fee
	err := writeElements(w,
		c.ReservationID,
		c.RequesterCloseSig,
		c.Fee)
	if err != nil {
		return err
	}

	return nil
}

func (c *CloseRequest) Command() uint32 {
	return CmdCloseRequest
}

func (c *CloseRequest) MaxPayloadLength(uint32) uint32 {
	//8 + 73 + 8
	return 89
}

//Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *CloseRequest) Validate() error {
	//Fee must be greater than 0
	if c.Fee < 0 {
		return fmt.Errorf("Fee must be greater than zero.")
	}
	//We're good!
	return nil
}

func (c *CloseRequest) String() string {
	var serializedSig []byte
	if c.RequesterCloseSig != nil && c.RequesterCloseSig.R != nil {
		serializedSig = c.RequesterCloseSig.Serialize()
	}

	return fmt.Sprintf("\n--- Begin CloseRequest ---\n") +
		fmt.Sprintf("ReservationID:\t\t%d\n", c.ReservationID) +
		fmt.Sprintf("CloseSig\t\t%x\n", serializedSig) +
		fmt.Sprintf("Fee:\t\t\t%d\n", c.Fee) +
		fmt.Sprintf("--- End CloseRequest ---\n")
}
