package lnwire

import (
	"bytes"
	"io"
)

// DynReject is a message that is sent during a dynamic commitments negotiation
// process. It is sent by both parties to propose new channel parameters.
type DynReject struct {
	// ChanID identifies the channel whose parameters we are trying to
	// re-negotiate.
	ChanID ChannelID

	// UpdateRejections is a bit vector that specifies which of the
	// DynPropose parameters we wish to call out as being unacceptable.
	UpdateRejections RawFeatureVector

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	//
	// NOTE: Since the fields in this structure are part of the TLV stream,
	// ExtraData will contain all TLV records _except_ the ones that are
	// present in earlier parts of this structure.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynReject implements the lnwire.Message
// interface.
var _ Message = (*DynReject)(nil)

// A compile time check to ensure DynReject implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*DynReject)(nil)

// Encode serializes the target DynReject into the passed io.Writer.
// Serialization will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dr *DynReject) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, dr.ChanID); err != nil {
		return err
	}

	if err := WriteRawFeatureVector(w, &dr.UpdateRejections); err != nil {
		return err
	}

	return WriteBytes(w, dr.ExtraData)
}

// Decode deserializes the serialized DynReject stored in the passed io.Reader
// into the target DynReject using the deserialization rules defined by the
// passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dr *DynReject) Decode(r io.Reader, _ uint32) error {
	var extra ExtraOpaqueData

	if err := ReadElements(
		r, &dr.ChanID, &dr.UpdateRejections, &extra,
	); err != nil {
		return err
	}

	if len(extra) != 0 {
		dr.ExtraData = extra
	}

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynReject on the wire.
//
// This is part of the lnwire.Message interface.
func (dr *DynReject) MsgType() MessageType {
	return MsgDynReject
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (dr *DynReject) SerializedSize() (uint32, error) {
	return MessageSerializedSize(dr)
}
