package wtwire

import "io"

// Error is a generic error message that can be sent to a client if a request
// fails outside of prescribed protocol errors. Typically this would be followed
// by the server disconnecting the client, and so can be useful to transferring
// the exact reason.
type Error struct {
	// Code specifies the error code encountered by the server.
	Code ErrorCode

	// Data encodes a payload whose contents can be interpreted by the
	// client in response to the error code.
	Data []byte
}

// NewError returns an freshly-initialized Error message.
func NewError() *Error {
	return &Error{}
}

// A compile time check to ensure Error implements the wtwire.Message interface.
var _ Message = (*Error)(nil)

// Decode deserializes a serialized Error message stored in the passed io.Reader
// observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (e *Error) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&e.Code,
		&e.Data,
	)
}

// Encode serializes the target Error into the passed io.Writer observing the
// protocol version specified.
//
// This is part of the wtwire.Message interface.
func (e *Error) Encode(w io.Writer, prver uint32) error {
	return WriteElements(w,
		e.Code,
		e.Data,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (e *Error) MsgType() MessageType {
	return MsgError
}

// MaxPayloadLength returns the maximum allowed payload size for a Error
// complete message observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (e *Error) MaxPayloadLength(uint32) uint32 {
	return MaxMessagePayload
}
