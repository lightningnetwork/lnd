package lnwire

import (
	"fmt"
	"io"
)

type HTLCAddAccept struct {
	ChannelID uint64
	StagingID uint64
}

func (c *HTLCAddAccept) Decode(r io.Reader, pver uint32) error {
	//ChannelID(8)
	//CommitmentHeight(8)
	//NextResponderCommitmentRevocationHash(20)
	//ResponderRevocationPreimage(20)
	//ResponderCommitSig(2+73max)
	err := readElements(r,
		&c.ChannelID,
		&c.StagingID,
	)
	if err != nil {
		return err
	}

	return nil
}

//Creates a new HTLCAddAccept
func NewHTLCAddAccept() *HTLCAddAccept {
	return &HTLCAddAccept{}
}

//Serializes the item from the HTLCAddAccept struct
//Writes the data to w
func (c *HTLCAddAccept) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.StagingID,
	)

	if err != nil {
		return err
	}

	return nil
}

func (c *HTLCAddAccept) Command() uint32 {
	return CmdHTLCAddAccept
}

func (c *HTLCAddAccept) MaxPayloadLength(uint32) uint32 {
	//16 base size
	return 16
}

//Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *HTLCAddAccept) Validate() error {
	//We're good!
	return nil
}

func (c *HTLCAddAccept) String() string {
	return fmt.Sprintf("\n--- Begin HTLCAddAccept ---\n") +
		fmt.Sprintf("ChannelID:\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("StagingID:\t\t%d\n", c.StagingID) +
		fmt.Sprintf("--- End HTLCAddAccept ---\n")
}
