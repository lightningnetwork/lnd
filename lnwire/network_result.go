package lnwire

import (
	"bytes"
	"io"
)

// PendingNetworkResult is a dummy message that implements the Message
// interface. It acts as a placeholder for the in-progress state of a locally
// initiated HTLC payment attempt.
type PendingNetworkResult struct{}

// MsgType returns a default MessageType.
func (e *PendingNetworkResult) MsgType() MessageType {
	return MsgPendingNetworkResult
}

// Decode is a no-op decoder for the empty message. Since this is just a placeholder,
// it doesn't actually decode any data.
func (e *PendingNetworkResult) Decode(r io.Reader, pver uint32) error {
	// No decoding necessary for an empty message.
	return nil
}

// Encode is a no-op encoder for the empty message. Since this is just a placeholder,
// it doesn't actually encode any data.
func (e *PendingNetworkResult) Encode(w *bytes.Buffer, pver uint32) error {
	// No encoding necessary for an empty message. The message type will
	// be encoded by the library and is enough to distinguish this from
	// other messages.
	return nil
}
