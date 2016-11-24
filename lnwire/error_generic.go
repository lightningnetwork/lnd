package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/wire"
	"google.golang.org/grpc/codes"
)

// ErrorCode represents the short error code for each of the defined errors
// within the Lightning Network protocol spec.
type ErrorCode uint16

// ToGrpcCode is used to generate gRPC specific code which will be propagated
// to the ln rpc client. This code is used to have more detailed view of what
// goes wrong and also in order to have the ability pragmatically determine
// the error and take specific actions on the client side.
func (e ErrorCode) ToGrpcCode() codes.Code {
	return (codes.Code)(e) + 100
}

const (
	// ErrorMaxPendingChannels is returned by remote peer when the number
	// of active pending channels exceeds their maximum policy limit.
	ErrorMaxPendingChannels ErrorCode = 1
)

// ErrorGeneric represents a generic error bound to an exact channel. The
// message format is purposefully general in order to allow expression of a wide
// array of possible errors. Each ErrorGeneric message is directed at a particular
// open channel referenced by ChannelPoint.
type ErrorGeneric struct {
	// ChannelPoint references the active channel in which the error
	// occurred within. A ChannelPoint of zeroHash:0 denotes this error
	// applies to the entire established connection.
	ChannelPoint *wire.OutPoint

	// PendingChannelID allows peers communicate errors in the context of a
	// particular pending channel. With this field, once a peer reads an
	// ErrorGeneric message with the PendingChannelID field set, then they
	// can forward the error to the fundingManager who can handle it
	// properly.
	PendingChannelID uint64

	// Code is the short error ID which describes the nature of the error.
	Code ErrorCode

	// Problem is a human-readable string further elaborating upon the
	// nature of the exact error. The maximum allowed length of this
	// message is 8192 bytes.
	Problem string
}

// NewErrorGeneric creates a new ErrorGeneric message.
func NewErrorGeneric() *ErrorGeneric {
	return &ErrorGeneric{}
}

// A compile time check to ensure ErrorGeneric implements the lnwire.Message
// interface.
var _ Message = (*ErrorGeneric)(nil)

// Decode deserializes a serialized ErrorGeneric message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ErrorGeneric) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint(8)
	// Problem
	err := readElements(r,
		&c.ChannelPoint,
		&c.Code,
		&c.Problem,
		&c.PendingChannelID,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target ErrorGeneric into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ErrorGeneric) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelPoint,
		c.Code,
		c.Problem,
		c.PendingChannelID,
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
		fmt.Sprintf("ChannelPoint:\t%d\n", c.ChannelPoint) +
		fmt.Sprintf("Code:\t%d\n", c.Code) +
		fmt.Sprintf("Problem:\t%s\n", c.Problem) +
		fmt.Sprintf("PendingChannelID:\t%s\n", c.PendingChannelID) +
		fmt.Sprintf("--- End ErrorGeneric ---\n")
}
