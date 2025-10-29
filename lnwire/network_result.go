package lnwire

import (
	"bytes"
	"io"
)

// PendingNetworkResult is a dummy message that implements the Message
// interface. It acts as a placeholder for the in-progress state of a locally
// initiated HTLC payment attempt.
type PendingNetworkResult struct{}

// A compile time check to ensure PendingNetworkResult implements the
// lnwire.Message interface.
var _ Message = (*PendingNetworkResult)(nil)

// A compile time check to ensure PendingNetworkResult implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*PendingNetworkResult)(nil)

// MsgType returns a default MessageType.
func (p *PendingNetworkResult) MsgType() MessageType {
	return MsgPendingNetworkResult
}

// Decode is a no-op decoder for the empty message. Since this is just a
// placeholder, it doesn't actually decode any data.
func (p *PendingNetworkResult) Decode(r io.Reader, pver uint32) error {
	// No decoding necessary for an empty message.
	return nil
}

// Encode is a no-op encoder for the empty message. Since this is just a
// placeholder, it doesn't actually encode any data.
func (p *PendingNetworkResult) Encode(w *bytes.Buffer, pver uint32) error {
	// No encoding necessary for an empty message. The message type will
	// be encoded by the library and is enough to distinguish this from
	// other messages.
	return nil
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (p *PendingNetworkResult) SerializedSize() (uint32, error) {
	return MessageSerializedSize(p)
}
