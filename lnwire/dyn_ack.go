package lnwire

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DALocalMusig2Pubnonce is the TLV type number that identifies the
	// musig2 public nonce that we need to verify the commitment transaction
	// signature.
	DALocalMusig2Pubnonce tlv.Type = 14
)

// DynAck is the message used to accept the parameters of a dynamic commitment
// negotiation. Additional optional parameters will need to be present depending
// on the details of the dynamic commitment upgrade.
type DynAck struct {
	// ChanID is the ChannelID of the channel that is currently undergoing
	// a dynamic commitment negotiation
	ChanID ChannelID

	// Sig is a signature that acknowledges and approves the parameters
	// that were requested in the DynPropose
	Sig Sig

	// LocalNonce is an optional field that is transmitted when accepting
	// a dynamic commitment upgrade to Taproot Channels. This nonce will be
	// used to verify the first commitment transaction signature. This will
	// only be populated if the DynPropose we are responding to specifies
	// taproot channels in the ChannelType field.
	LocalNonce tlv.OptionalRecordT[tlv.TlvType14, Musig2Nonce]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynAck implements the lnwire.Message
// interface.
var _ Message = (*DynAck)(nil)

// A compile time check to ensure DynAck implements the lnwire.SizeableMessage
// interface.
var _ SizeableMessage = (*DynAck)(nil)

// Encode serializes the target DynAck into the passed io.Writer. Serialization
// will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (da *DynAck) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, da.ChanID); err != nil {
		return err
	}

	if err := WriteSig(w, da.Sig); err != nil {
		return err
	}

	// Create extra data records.
	producers, err := da.ExtraData.RecordProducers()
	if err != nil {
		return err
	}

	// Append the known records.
	da.LocalNonce.WhenSome(
		func(rec tlv.RecordT[tlv.TlvType14, Musig2Nonce]) {
			producers = append(producers, &rec)
		},
	)

	// Encode all records.
	var tlvData ExtraOpaqueData
	err = tlvData.PackRecords(producers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, tlvData)
}

// Decode deserializes the serialized DynAck stored in the passed io.Reader into
// the target DynAck using the deserialization rules defined by the passed
// protocol version.
//
// This is a part of the lnwire.Message interface.
func (da *DynAck) Decode(r io.Reader, _ uint32) error {
	// Parse out main message.
	if err := ReadElements(r, &da.ChanID, &da.Sig); err != nil {
		return err
	}

	// Parse out TLV records.
	var tlvRecords ExtraOpaqueData
	if err := ReadElement(r, &tlvRecords); err != nil {
		return err
	}

	// Parse all known records and extra data.
	nonce := da.LocalNonce.Zero()
	knownRecords, extraData, err := ParseAndExtractExtraData(
		tlvRecords, &nonce,
	)
	if err != nil {
		return err
	}

	// Check the results of the TLV Stream decoding and appropriately set
	// message fields.
	if _, ok := knownRecords[da.LocalNonce.TlvType()]; ok {
		da.LocalNonce = tlv.SomeRecordT(nonce)
	}

	da.ExtraData = extraData

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynAck on the wire.
//
// This is part of the lnwire.Message interface.
func (da *DynAck) MsgType() MessageType {
	return MsgDynAck
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (da *DynAck) SerializedSize() (uint32, error) {
	return MessageSerializedSize(da)
}
