package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// ClosingSigs houses the 3 possible signatures that can be sent when
// attempting to complete a cooperative channel closure. A signature will
// either include both outputs, or only one of the outputs from either side.
type ClosingSigs struct {
	// CloserNoClosee is a signature that excludes the output of the
	// clsoee.
	CloserNoClosee tlv.OptionalRecordT[tlv.TlvType1, Sig]

	// NoCloserClosee is a signature that excludes the output of the
	// closer.
	NoCloserClosee tlv.OptionalRecordT[tlv.TlvType2, Sig]

	// CloserAndClosee is a signature that includes both outputs.
	CloserAndClosee tlv.OptionalRecordT[tlv.TlvType3, Sig]
}

// ClosingComplete is sent by either side to kick off the process of obtaining
// a valid signature on a c o-operative channel closure of their choice.
type ClosingComplete struct {
	// ChannelID serves to identify which channel is to be closed.
	ChannelID ChannelID

	// CloserScript is the script to which the channel funds will be paid
	// for the closer (the person sending the ClosingComplete) message.
	CloserScript DeliveryAddress

	// CloseeScript is the script to which the channel funds will be paid
	// (the person receiving the ClosingComplete message).
	CloseeScript DeliveryAddress

	// FeeSatoshis is the total fee in satoshis that the party to the
	// channel would like to propose for the close transaction.
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

// decodeClosingSigs decodes the closing sig TLV records in the passed
// ExtraOpaqueData.
func decodeClosingSigs(c *ClosingSigs, tlvRecords ExtraOpaqueData) (
	ExtraOpaqueData, error) {

	sig1 := c.CloserNoClosee.Zero()
	sig2 := c.NoCloserClosee.Zero()
	sig3 := c.CloserAndClosee.Zero()

	knownRecords, extraData, err := ParseAndExtractExtraData(
		tlvRecords, &sig1, &sig2, &sig3,
	)
	if err != nil {
		return nil, err
	}

	if _, ok := knownRecords[c.CloserNoClosee.TlvType()]; ok {
		c.CloserNoClosee = tlv.SomeRecordT(sig1)
	}
	if _, ok := knownRecords[c.NoCloserClosee.TlvType()]; ok {
		c.NoCloserClosee = tlv.SomeRecordT(sig2)
	}
	if _, ok := knownRecords[c.CloserAndClosee.TlvType()]; ok {
		c.CloserAndClosee = tlv.SomeRecordT(sig3)
	}

	return extraData, nil
}

// Decode deserializes a serialized ClosingComplete message stored in the
// passed io.Reader.
func (c *ClosingComplete) Decode(r io.Reader, _ uint32) error {
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

	extraData, err := decodeClosingSigs(&c.ClosingSigs, tlvRecords)
	if err != nil {
		return err
	}

	c.ExtraData = extraData

	return nil
}

// closingSigRecords returns the set of records that encode the closing sigs,
// if present.
func closingSigRecords(c *ClosingSigs) []tlv.RecordProducer {
	recordProducers := make([]tlv.RecordProducer, 0, 3)
	c.CloserNoClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType1, Sig]) {
		recordProducers = append(recordProducers, &sig)
	})
	c.NoCloserClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType2, Sig]) {
		recordProducers = append(recordProducers, &sig)
	})
	c.CloserAndClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType3, Sig]) {
		recordProducers = append(recordProducers, &sig)
	})

	return recordProducers
}

// Encode serializes the target ClosingComplete into the passed io.Writer.
func (c *ClosingComplete) Encode(w *bytes.Buffer, _ uint32) error {
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

	// Get producers from extra data.
	producers, err := c.ExtraData.RecordProducers()
	if err != nil {
		return err
	}

	producers = append(producers, closingSigRecords(&c.ClosingSigs)...)

	// Pack all records into a new TLV stream.
	var tlvData ExtraOpaqueData
	err = tlvData.PackRecords(producers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, tlvData)
}

// MsgType returns the uint32 code which uniquely identifies this message as a
// ClosingComplete message on the wire.
//
// This is part of the lnwire.Message interface.
func (c *ClosingComplete) MsgType() MessageType {
	return MsgClosingComplete
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *ClosingComplete) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}

// A compile time check to ensure ClosingComplete implements the lnwire.Message
// interface.
var _ Message = (*ClosingComplete)(nil)

// A compile time check to ensure ClosingComplete implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*ClosingComplete)(nil)
