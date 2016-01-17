package lnwire

import (
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"

	"io"
)

type CloseComplete struct {
	ReservationID uint64

	ResponderCloseSig *btcec.Signature // Requester's Commitment
	CloseShaHash      *wire.ShaHash    // TxID of the Close Tx
}

func (c *CloseComplete) Decode(r io.Reader, pver uint32) error {
	// ReservationID (8)
	// ResponderCloseSig (73)
	// 	First byte length then sig
	// CloseShaHash (32)
	err := readElements(r,
		&c.ReservationID,
		&c.ResponderCloseSig,
		&c.CloseShaHash)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new CloseComplete
func NewCloseComplete() *CloseComplete {
	return &CloseComplete{}
}

// Serializes the item from the CloseComplete struct
// Writes the data to w
func (c *CloseComplete) Encode(w io.Writer, pver uint32) error {
	// ReservationID
	// ResponderCloseSig
	// CloseShaHash
	err := writeElements(w,
		c.ReservationID,
		c.ResponderCloseSig,
		c.CloseShaHash)
	if err != nil {
		return err
	}

	return nil
}

func (c *CloseComplete) Command() uint32 {
	return CmdCloseComplete
}

func (c *CloseComplete) MaxPayloadLength(uint32) uint32 {
	// 8 + 73 + 32
	return 113
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *CloseComplete) Validate() error {
	// We're good!
	return nil
}

func (c *CloseComplete) String() string {
	var serializedSig []byte
	var shaString string
	if c.ResponderCloseSig != nil && c.ResponderCloseSig.R != nil {
		serializedSig = c.ResponderCloseSig.Serialize()
	}
	if c.CloseShaHash != nil {
		shaString = (*c).CloseShaHash.String()
	}

	return fmt.Sprintf("\n--- Begin CloseComplete ---\n") +
		fmt.Sprintf("ReservationID:\t\t%d\n", c.ReservationID) +
		fmt.Sprintf("ResponderCloseSig:\t%x\n", serializedSig) +
		fmt.Sprintf("CloseShaHash:\t\t%s\n", shaString) +
		fmt.Sprintf("--- End CloseComplete ---\n")
}
