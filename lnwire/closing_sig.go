package lnwire

import (
	"bytes"
	"io"
)

// ClosingSig is sent in response to a ClosingComplete message. It carries the
// signatures of the closee to the closer.
type ClosingSig struct {
	// ChannelID serves to identify which channel is to be closed.
	ChannelID ChannelID

	// ClosingSigs houses the 3 possible signatures that can be sent.
	ClosingSigs

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// Decode deserializes a serialized ClosingSig message stored in the passed
// io.Reader.
func (c *ClosingSig) Decode(r io.Reader, _ uint32) error {
	// First, read out all the fields that are hard coded into the message.
	err := ReadElements(r, &c.ChannelID)
	if err != nil {
		return err
	}

	// With the hard coded messages read, we'll now read out the TLV fields
	// of the message.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	if err := decodeClosingSigs(&c.ClosingSigs, tlvRecords); err != nil {
		return err
	}

	if len(tlvRecords) != 0 {
		c.ExtraData = tlvRecords
	}

	return nil
}

// Encode serializes the target ClosingSig into the passed io.Writer.
func (c *ClosingSig) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, c.ChannelID); err != nil {
		return err
	}

	recordProducers := closingSigRecords(&c.ClosingSigs)

	err := EncodeMessageExtraData(&c.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, c.ExtraData)
}

// MsgType returns the uint32 code which uniquely identifies this message as a
// ClosingSig message on the wire.
//
// This is part of the lnwire.Message interface.
func (c *ClosingSig) MsgType() MessageType {
	return MsgClosingSig
}

// A compile time check to ensure ClosingSig implements the lnwire.Message
// interface.
var _ Message = (*ClosingSig)(nil)
