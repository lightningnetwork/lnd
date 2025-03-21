package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
)

// ClosingSig is sent in response to a ClosingComplete message. It carries the
// signatures of the closee to the closer.
type ClosingSig struct {
	// ChannelID serves to identify which channel is to be closed.
	ChannelID ChannelID

	// CloserScript is the script to which the channel funds will be paid
	// for the closer (the person sending the ClosingComplete) message.
	CloserScript DeliveryAddress

	// CloseeScript is the script to which the channel funds will be paid
	// (the person receiving the ClosingComplete message).
	CloseeScript DeliveryAddress

	// FeeSatoshis is the total fee in satoshis that the party to the
	// channel proposed for the close transaction.
	FeeSatoshis btcutil.Amount

	// LockTime is the locktime number to be used in the input spending the
	// funding transaction.
	LockTime uint32

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
	err := ReadElements(
		r, &c.ChannelID, &c.CloserScript, &c.CloseeScript,
		&c.FeeSatoshis, &c.LockTime,
	)
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

	if err := WriteDeliveryAddress(w, c.CloserScript); err != nil {
		return err
	}
	if err := WriteDeliveryAddress(w, c.CloseeScript); err != nil {
		return err
	}

	if err := WriteSatoshi(w, c.FeeSatoshis); err != nil {
		return err
	}

	if err := WriteUint32(w, c.LockTime); err != nil {
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

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *ClosingSig) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}

// A compile time check to ensure ClosingSig implements the lnwire.Message
// interface.
var _ Message = (*ClosingSig)(nil)

// A compile time check to ensure ClosingSig implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*ClosingSig)(nil)
