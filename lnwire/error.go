package lnwire

import (
	"io"

	"google.golang.org/grpc/codes"
)

// ErrorCode represents the short error code for each of the defined errors
// within the Lightning Network protocol spec.
type ErrorCode uint8

// ToGrpcCode is used to generate gRPC specific code which will be propagated
// to the ln rpc client. This code is used to have more detailed view of what
// goes wrong and also in order to have the ability pragmatically determine the
// error and take specific actions on the client side.
func (e ErrorCode) ToGrpcCode() codes.Code {
	return (codes.Code)(e) + 100
}

const (
	// ErrMaxPendingChannels is returned by remote peer when the number of
	// active pending channels exceeds their maximum policy limit.
	ErrMaxPendingChannels ErrorCode = 1

	// ErrSynchronizingChain is returned by a remote peer that receives a
	// channel update or a funding request while their still syncing to the
	// latest state of the blockchain.
	ErrSynchronizingChain ErrorCode = 2

	// ErrChanTooLarge is returned by a remote peer that receives a
	// FundingOpen request for a channel that is above their current
	// soft-limit.
	ErrChanTooLarge ErrorCode = 3
)

// String returns a human readable version of the target ErrorCode.
func (e ErrorCode) String() string {
	switch e {
	case ErrMaxPendingChannels:
		return "Number of pending channels exceed maximum"
	case ErrSynchronizingChain:
		return "Synchronizing blockchain"
	case ErrChanTooLarge:
		return "channel too large"
	default:
		return "unknown error"
	}
}

// Error returns the human redable version of the target ErrorCode.
//
// Satisfies the Error interface.
func (e ErrorCode) Error() string {
	return e.String()
}

// ErrorData is a set of bytes associated with a particular sent error. A
// receiving node SHOULD only print out data verbatim if the string is composed
// solely of printable ASCII characters. For reference, the printable character
// set includes byte values 32 through 127 inclusive.
type ErrorData []byte

// Error represents a generic error bound to an exact channel. The message
// format is purposefully general in order to allow expression of a wide array
// of possible errors. Each Error message is directed at a particular open
// channel referenced by ChannelPoint.
//
// TODO(roasbeef): remove the error code
type Error struct {
	// ChanID references the active channel in which the error occurred
	// within. If the ChanID is all zeros, then this error applies to the
	// entire established connection.
	ChanID ChannelID

	// Data is the attached error data that describes the exact failure
	// which caused the error message to be sent.
	Data ErrorData
}

// NewError creates a new Error message.
func NewError() *Error {
	return &Error{}
}

// A compile time check to ensure Error implements the lnwire.Message
// interface.
var _ Message = (*Error)(nil)

// Decode deserializes a serialized Error message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *Error) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&c.ChanID,
		&c.Data,
	)
}

// Encode serializes the target Error into the passed io.Writer observing the
// protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *Error) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.ChanID,
		c.Data,
	)
}

// MsgType returns the integer uniquely identifying an Error message on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *Error) MsgType() MessageType {
	return MsgError
}

// MaxPayloadLength returns the maximum allowed payload size for an Error
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *Error) MaxPayloadLength(uint32) uint32 {
	// 32 + 2 + 655326
	return 65536
}
