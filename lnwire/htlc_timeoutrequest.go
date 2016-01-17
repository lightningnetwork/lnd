package lnwire

import (
	"fmt"
	"io"
)

// Multiple Clearing Requests are possible by putting this inside an array of
// clearing requests
type HTLCTimeoutRequest struct {
	// We can use a different data type for this if necessary...
	ChannelID uint64

	// ID of this request
	HTLCKey HTLCKey
}

func (c *HTLCTimeoutRequest) Decode(r io.Reader, pver uint32) error {
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

// Creates a new HTLCTimeoutRequest
func NewHTLCTimeoutRequest() *HTLCTimeoutRequest {
	return &HTLCTimeoutRequest{}
}

// Serializes the item from the HTLCTimeoutRequest struct
// Writes the data to w
func (c *HTLCTimeoutRequest) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.HTLCKey,
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *HTLCTimeoutRequest) Command() uint32 {
	return CmdHTLCTimeoutRequest
}

func (c *HTLCTimeoutRequest) MaxPayloadLength(uint32) uint32 {
	// 16
	return 16
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *HTLCTimeoutRequest) Validate() error {
	// We're good!
	return nil
}

func (c *HTLCTimeoutRequest) String() string {
	return fmt.Sprintf("\n--- Begin HTLCTimeoutRequest ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("--- End HTLCTimeoutRequest ---\n")
}
