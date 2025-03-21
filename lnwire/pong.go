package lnwire

import (
	"bytes"
	"fmt"
	"io"
)

// MaxPongBytes is the maximum number of extra bytes a pong can be requested to
// send. The type of the message (19) takes 2 bytes, the length field takes up
// 2 bytes, leaving 65531 bytes.
const MaxPongBytes = 65531

// ErrMaxPongBytesExceeded indicates that the NumPongBytes field from the ping
// message has exceeded MaxPongBytes.
var ErrMaxPongBytesExceeded = fmt.Errorf("pong bytes exceeded")

// PongPayload is a set of opaque bytes sent in response to a ping message.
type PongPayload []byte

// Pong defines a message which is the direct response to a received Ping
// message. A Pong reply indicates that a connection is still active. The Pong
// reply to a Ping message should contain the nonce carried in the original
// Pong message.
type Pong struct {
	// PongBytes is a set of opaque bytes that corresponds to the
	// NumPongBytes defined in the ping message that this pong is
	// replying to.
	PongBytes PongPayload
}

// NewPong returns a new Pong message.
func NewPong(pongBytes []byte) *Pong {
	return &Pong{
		PongBytes: pongBytes,
	}
}

// A compile time check to ensure Pong implements the lnwire.Message interface.
var _ Message = (*Pong)(nil)

// A compile time check to ensure Pong implements the lnwire.SizeableMessage
// interface.
var _ SizeableMessage = (*Pong)(nil)

// Decode deserializes a serialized Pong message stored in the passed io.Reader
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (p *Pong) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&p.PongBytes,
	)
}

// Encode serializes the target Pong into the passed io.Writer observing the
// protocol version specified.
//
// This is part of the lnwire.Message interface.
func (p *Pong) Encode(w *bytes.Buffer, pver uint32) error {
	return WritePongPayload(w, p.PongBytes)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (p *Pong) MsgType() MessageType {
	return MsgPong
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (p *Pong) SerializedSize() (uint32, error) {
	return MessageSerializedSize(p)
}
