package lnwire

import (
	"fmt"
	"io"
)

// ErrorGeneric ...
// Multiple Clearing Requests are possible by putting this inside an array of
// clearing requests
type ErrorGeneric struct {
	// We can use a different data type for this if necessary...
	ChannelID uint64
	// Some kind of message
	// Max length 8192
	Problem string
}

// Decode ...
func (c *ErrorGeneric) Decode(r io.Reader, pver uint32) error {
	// ChannelID(8)
	// Problem
	err := readElements(r,
		&c.ChannelID,
		&c.Problem,
	)
	if err != nil {
		return err
	}

	return nil
}

// NewErrorGeneric creates a new ErrorGeneric
func NewErrorGeneric() *ErrorGeneric {
	return &ErrorGeneric{}
}

// Encode serializes the item from the ErrorGeneric struct
// Writes the data to w
func (c *ErrorGeneric) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.Problem,
	)
	if err != nil {
		return err
	}

	return nil
}

// Command ...
func (c *ErrorGeneric) Command() uint32 {
	return CmdErrorGeneric
}

// MaxPayloadLength ...
func (c *ErrorGeneric) MaxPayloadLength(uint32) uint32 {
	// 8+8192
	return 8208
}

// Validate makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *ErrorGeneric) Validate() error {
	if len(c.Problem) > 8192 {
		return fmt.Errorf("Problem string length too long")
	}
	// We're good!
	return nil
}

func (c *ErrorGeneric) String() string {
	return fmt.Sprintf("\n--- Begin ErrorGeneric ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("Problem:\t%s\n", c.Problem) +
		fmt.Sprintf("--- End ErrorGeneric ---\n")
}
