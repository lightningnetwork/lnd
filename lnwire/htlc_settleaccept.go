package lnwire

import (
	"fmt"
	"io"
)

// Multiple Clearing Requests are possible by putting this inside an array of
// clearing requests
type HTLCSettleAccept struct {
	// We can use a different data type for this if necessary...
	ChannelID uint64

	// ID of this request
	HTLCKey HTLCKey
}

func (c *HTLCSettleAccept) Decode(r io.Reader, pver uint32) error {
	// ChannelID(8)
	// HTLCKey(8)
	err := readElements(r,
		&c.ChannelID,
		&c.HTLCKey,
	)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new HTLCSettleAccept
func NewHTLCSettleAccept() *HTLCSettleAccept {
	return &HTLCSettleAccept{}
}

// Serializes the item from the HTLCSettleAccept struct
// Writes the data to w
func (c *HTLCSettleAccept) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.HTLCKey,
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *HTLCSettleAccept) Command() uint32 {
	return CmdHTLCSettleAccept
}

func (c *HTLCSettleAccept) MaxPayloadLength(uint32) uint32 {
	// 16
	return 16
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *HTLCSettleAccept) Validate() error {
	// We're good!
	return nil
}

func (c *HTLCSettleAccept) String() string {
	return fmt.Sprintf("\n--- Begin HTLCSettleAccept ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("--- End HTLCSettleAccept ---\n")
}
