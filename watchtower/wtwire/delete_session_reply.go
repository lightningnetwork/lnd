package wtwire

import "io"

// DeleteSessionCode is an error code returned by a watchtower in response to a
// DeleteSession message.
type DeleteSessionCode = ErrorCode

const (
	// DeleteSessionCodeNotFound is returned when the watchtower does not
	// know of the requested session. This may indicate an error on the
	// client side, or that the tower had already deleted the session in a
	// prior request that the client may not have received.
	DeleteSessionCodeNotFound DeleteSessionCode = 80

	// TODO(conner): add String method after wtclient is merged
)

// DeleteSessionReply is a message sent in response to a client's DeleteSession
// request. The message indicates whether or not the deletion was a success or
// failure.
type DeleteSessionReply struct {
	// Code will be non-zero if the watchtower was not able to delete the
	// requested session.
	Code DeleteSessionCode
}

// A compile time check to ensure DeleteSessionReply implements the
// wtwire.Message interface.
var _ Message = (*DeleteSessionReply)(nil)

// Decode deserializes a serialized DeleteSessionReply message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *DeleteSessionReply) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&m.Code,
	)
}

// Encode serializes the target DeleteSessionReply into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the wtwire.Message interface.
func (m *DeleteSessionReply) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		m.Code,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (m *DeleteSessionReply) MsgType() MessageType {
	return MsgDeleteSessionReply
}

// MaxPayloadLength returns the maximum allowed payload size for a
// DeleteSessionReply complete message observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *DeleteSessionReply) MaxPayloadLength(uint32) uint32 {
	return 2
}
