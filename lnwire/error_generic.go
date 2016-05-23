package lnwire

import (
	"fmt"
	"io"
)

// ErrorGeneric represents a generic error bound to an exact channel. The
// message format is purposefully general in order to allow expressino of a wide
// array of possible errors. Each ErrorGeneric message is directed at a particular
// open channel referenced by ChannelID.
type ErrorGeneric struct {
	// ChannelID references the active channel in which the error occured
	// within.
	ChannelID uint64

	// TODO(roasbeef): uint16 for problem type?
	// ErrorID uint16

	// Problem is a human-readable string further elaborating upon the
	// nature of the exact error. The maxmium allowed length of this
	// message is 8192 bytes.
	Problem string

	// TODO(roasbeef): add SerializeSize?
}

// Decode deserializes a serialized ErrorGeneric message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
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

// NewErrorGeneric creates a new ErrorGeneric message.
func NewErrorGeneric() *ErrorGeneric {
	return &ErrorGeneric{}
}

// A compile time check to ensure ErrorGeneric implements the lnwire.Message
// interface.
var _ Message = (*ErrorGeneric)(nil)

// Encode serializes the target ErrorGeneric into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
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

// Command returns the integer uniquely identifying an ErrorGeneric message on
// the wire.
//
// This is part of the lnwire.Message interface.
func (c *ErrorGeneric) Command() uint32 {
	return CmdErrorGeneric
}

// MaxPayloadLength returns the maximum allowed payload size for a
// ErrorGeneric complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ErrorGeneric) MaxPayloadLength(uint32) uint32 {
	// 8+8192
	return 8208
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the ErrorGeneric are valid.
//
// This is part of the lnwire.Message interface.
func (c *ErrorGeneric) Validate() error {
	if len(c.Problem) > 8192 {
		return fmt.Errorf("problem string length too long")
	}

	// We're good!
	return nil
}

// String returns the string representation of the target ErrorGeneric.
//
// This is part of the lnwire.Message interface.
func (c *ErrorGeneric) String() string {
	return fmt.Sprintf("\n--- Begin ErrorGeneric ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("Problem:\t%s\n", c.Problem) +
		fmt.Sprintf("--- End ErrorGeneric ---\n")
}
