package lnwire

import (
	"fmt"
	"io"
)

// Multiple Clearing Requests are possible by putting this inside an array of
// clearing requests
type HTLCTimeoutAccept struct {
	// We can use a different data type for this if necessary...
	ChannelID uint64

	// ID of this request
	HTLCKey HTLCKey
}

func (c *HTLCTimeoutAccept) Decode(r io.Reader, pver uint32) error {
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

// Creates a new HTLCTimeoutAccept
func NewHTLCTimeoutAccept() *HTLCTimeoutAccept {
	return &HTLCTimeoutAccept{}
}

// Serializes the item from the HTLCTimeoutAccept struct
// Writes the data to w
func (c *HTLCTimeoutAccept) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.HTLCKey,
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *HTLCTimeoutAccept) Command() uint32 {
	return CmdHTLCTimeoutAccept
}

func (c *HTLCTimeoutAccept) MaxPayloadLength(uint32) uint32 {
	// 16
	return 16
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *HTLCTimeoutAccept) Validate() error {
	// We're good!
	return nil
}

func (c *HTLCTimeoutAccept) String() string {
	return fmt.Sprintf("\n--- Begin HTLCTimeoutAccept ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("--- End HTLCTimeoutAccept ---\n")
}
