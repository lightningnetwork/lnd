package lnwire

import (
	"bytes"
	"io"
)

// DynAck is the message used to accept the parameters of a dynamic commitment
// negotiation. Additional optional parameters will need to be present depending
// on the details of the dynamic commitment upgrade.
type DynAck struct {
	// ChanID is the ChannelID of the channel that is currently undergoing
	// a dynamic commitment negotiation
	ChanID ChannelID

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynAck implements the lnwire.Message
// interface.
var _ Message = (*DynAck)(nil)

// Encode serializes the target DynAck into the passed io.Writer. Serialization
// will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (da *DynAck) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, da.ChanID); err != nil {
		return err
	}

	return WriteBytes(w, da.ExtraData)
}

// Decode deserializes the serialized DynAck stored in the passed io.Reader into
// the target DynAck using the deserialization rules defined by the passed
// protocol version.
//
// This is a part of the lnwire.Message interface.
func (da *DynAck) Decode(r io.Reader, _ uint32) error {
	return ReadElements(r, &da.ChanID, &da.ExtraData)
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynAck on the wire.
//
// This is part of the lnwire.Message interface.
func (da *DynAck) MsgType() MessageType {
	return MsgDynAck
}
