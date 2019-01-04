package lnwire

import (
	"io"
)

// Shutdown is sent by either side in order to initiate the cooperative closure
// of a channel. This message is sparse as both sides implicitly have the
// information necessary to construct a transaction that will send the settled
// funds of both parties to the final delivery addresses negotiated during the
// funding workflow.
type Shutdown struct {
	// ChannelID serves to identify which channel is to be closed.
	ChannelID ChannelID

	// Address is the script to which the channel funds will be paid.
	Address DeliveryAddress
}

// DeliveryAddress is used to communicate the address to which funds from a
// closed channel should be sent. The address can be a p2wsh, p2pkh, p2sh or
// p2wpkh.
type DeliveryAddress []byte

// NewShutdown creates a new Shutdown message.
func NewShutdown(cid ChannelID, addr DeliveryAddress) *Shutdown {
	return &Shutdown{
		ChannelID: cid,
		Address:   addr,
	}
}

// A compile-time check to ensure Shutdown implements the lnwire.Message
// interface.
var _ Message = (*Shutdown)(nil)

// Decode deserializes a serialized Shutdown stored in the passed io.Reader
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r, &s.ChannelID, &s.Address)
}

// Encode serializes the target Shutdown into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w, s.ChannelID, s.Address)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) MsgType() MessageType {
	return MsgShutdown
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) MaxPayloadLength(pver uint32) uint32 {
	var length uint32

	// ChannelID - 32bytes
	length += 32

	// Len - 2 bytes
	length += 2

	// ScriptPubKey - 34 bytes for pay to witness script hash
	length += 34

	// NOTE: pay to pubkey hash is 25 bytes, pay to script hash is 22
	// bytes, and pay to witness pubkey hash is 22 bytes in length.

	return length
}
