package lnwire

import (
	"fmt"
	"io"
)

type HTLCAddReject struct {
	ChannelID uint64
	HTLCKey HTLCKey
}

func (c *HTLCAddReject) Decode(r io.Reader, pver uint32) error {
	//ChannelID(8)
	//CommitmentHeight(8)
	//NextResponderCommitmentRevocationHash(20)
	//ResponderRevocationPreimage(20)
	//ResponderCommitSig(2+73max)
	err := readElements(r,
		&c.ChannelID,
		&c.HTLCKey,
	)
	if err != nil {
		return err
	}

	return nil
}

//Creates a new HTLCAddReject
func NewHTLCAddReject() *HTLCAddReject {
	return &HTLCAddReject{}
}

//Serializes the item from the HTLCAddReject struct
//Writes the data to w
func (c *HTLCAddReject) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.HTLCKey,
	)

	if err != nil {
		return err
	}

	return nil
}

func (c *HTLCAddReject) Command() uint32 {
	return CmdHTLCAddReject
}

func (c *HTLCAddReject) MaxPayloadLength(uint32) uint32 {
	//16 base size
	return 16
}

//Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *HTLCAddReject) Validate() error {
	//We're good!
	return nil
}

func (c *HTLCAddReject) String() string {
	return fmt.Sprintf("\n--- Begin HTLCAddReject ---\n") +
		fmt.Sprintf("ChannelID:\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t\t%d\n", c.HTLCKey) +
		fmt.Sprintf("--- End HTLCAddReject ---\n")
}
