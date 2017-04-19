package lnwire

import (
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"

	"io"
)

// CloseComplete is sent by Bob signalling a fufillment and completion of
// Alice's prior CloseRequest message. After Alice receives Bob's CloseComplete
// message, she is able to broadcast the fully signed transaction executing a
// cooperative closure of the channel.
//
// NOTE: The responder is able to only send a signature without any additional
// message as all transactions are assembled observing BIP 69 which defines a
// cannonical ordering for input/outputs. Therefore, both sides are able to
// arrive at an identical closure transaction as they know the order of the
// inputs/outputs.
type CloseComplete struct {
	// ChannelPoint serves to identify which channel is to be closed.
	ChannelPoint wire.OutPoint

	// ResponderCloseSig is the signature of the responder for the
	// transaction which closes the previously active channel.
	ResponderCloseSig *btcec.Signature
}

// NewCloseComplete creates a new empty CloseComplete message.
// TODO(roasbeef): add params to all constructors...
func NewCloseComplete() *CloseComplete {
	return &CloseComplete{}
}

// A compile time check to ensure CloseComplete implements the lnwire.Message
// interface.
var _ Message = (*CloseComplete)(nil)

// Decode deserializes a serialized CloseComplete message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CloseComplete) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&c.ChannelPoint,
		&c.ResponderCloseSig)
}

// Encode serializes the target CloseComplete into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CloseComplete) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.ChannelPoint,
		c.ResponderCloseSig)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *CloseComplete) MsgType() MessageType {
	return MsgCloseComplete
}

// MaxPayloadLength returns the maximum allowed payload size for a CloseComplete
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CloseComplete) MaxPayloadLength(uint32) uint32 {
	// 141 + 73 + 32
	return 141
}
