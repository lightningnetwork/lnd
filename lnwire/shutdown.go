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

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// DeliveryAddress is used to communicate the address to which funds from a
// closed channel should be sent. The address can be a p2wsh, p2pkh, p2sh or
// p2wpkh.
type DeliveryAddress []byte

// deliveryAddressMaxSize is the maximum expected size in bytes of a
// DeliveryAddress based on the types of scripts we know.
// Following are the known scripts and their sizes in bytes.
// - pay to witness script hash: 34
// - pay to pubkey hash: 25
// - pay to script hash: 22
// - pay to witness pubkey hash: 22.
const deliveryAddressMaxSize = 34

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
	return ReadElements(r, &s.ChannelID, &s.Address, &s.ExtraData)
}

// Encode serializes the target Shutdown into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w, s.ChannelID, s.Address, s.ExtraData)
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
	return MaxMsgBody
}
