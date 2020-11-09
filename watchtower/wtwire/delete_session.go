package wtwire

import "io"

// DeleteSession is sent from the client to the tower to signal that the tower
// can delete all session state for the session key used to authenticate the
// brontide connection. This should be done by the client once all channels that
// have state updates in the session have been resolved on-chain.
type DeleteSession struct{}

// Compile-time constraint to ensure DeleteSession implements the wtwire.Message
// interface.
var _ Message = (*DeleteSession)(nil)

// Decode deserializes a serialized DeleteSession message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *DeleteSession) Decode(r io.Reader, pver uint32) error {
	return nil
}

// Encode serializes the target DeleteSession message into the passed io.Writer
// observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *DeleteSession) Encode(w io.Writer, pver uint32) error {
	return nil
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (m *DeleteSession) MsgType() MessageType {
	return MsgDeleteSession
}

// MaxPayloadLength returns the maximum allowed payload size for a DeleteSession
// message observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (m *DeleteSession) MaxPayloadLength(uint32) uint32 {
	return 0
}
