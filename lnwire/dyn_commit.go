package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// DynCommit is a composite message that is used to irrefutably execute a
// dynamic commitment update.
type DynCommit struct {
	// DynPropose is an embedded version of the original DynPropose message
	// that initiated this negotiation.
	DynPropose

	// DynAck is an embedded version of the original DynAck message that
	// countersigned this negotiation.
	DynAck

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynCommit implements the lnwire.Message
// interface.
var _ Message = (*DynCommit)(nil)

// A compile time check to ensure DynCommit implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*DynCommit)(nil)

// Encode serializes the target DynAck into the passed io.Writer. Serialization
// will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dc *DynCommit) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, dc.DynPropose.ChanID); err != nil {
		return err
	}

	if err := WriteSig(w, dc.Sig); err != nil {
		return err
	}

	var extra ExtraOpaqueData
	err := extra.PackRecords(dynProposeRecords(&dc.DynPropose)...)
	if err != nil {
		return err
	}
	dc.ExtraData = extra

	return WriteBytes(w, dc.ExtraData)
}

// Decode deserializes the serialized DynCommit stored in the passed io.Reader
// into the target DynAck using the deserialization rules defined by the passed
// protocol version.
//
// This is a part of the lnwire.Message interface.
func (dc *DynCommit) Decode(r io.Reader, _ uint32) error {
	// Parse out main message.
	if err := ReadElements(r, &dc.DynPropose.ChanID, &dc.Sig); err != nil {
		return err
	}
	dc.DynAck.ChanID = dc.DynPropose.ChanID

	// Parse out TLV records.
	var tlvRecords ExtraOpaqueData
	if err := ReadElement(r, &tlvRecords); err != nil {
		return err
	}

	// Prepare receiving buffers to be filled by TLV extraction.
	var dustLimit tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
	var maxValue tlv.RecordT[tlv.TlvType2, MilliSatoshi]
	var htlcMin tlv.RecordT[tlv.TlvType4, MilliSatoshi]
	var reserve tlv.RecordT[tlv.TlvType6, tlv.BigSizeT[btcutil.Amount]]
	csvDelay := dc.CsvDelay.Zero()
	maxHtlcs := dc.MaxAcceptedHTLCs.Zero()
	chanType := dc.ChannelType.Zero()

	typeMap, err := tlvRecords.ExtractRecords(
		&dustLimit, &maxValue, &htlcMin, &reserve, &csvDelay, &maxHtlcs,
		&chanType,
	)
	if err != nil {
		return err
	}

	// Check the results of the TLV Stream decoding and appropriately set
	// message fields.
	if val, ok := typeMap[dc.DustLimit.TlvType()]; ok && val == nil {
		var rec tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
		rec.Val = dustLimit.Val
		dc.DustLimit = tlv.SomeRecordT(rec)
	}
	if val, ok := typeMap[dc.MaxValueInFlight.TlvType()]; ok && val == nil {
		var rec tlv.RecordT[tlv.TlvType2, MilliSatoshi]
		rec.Val = maxValue.Val
		dc.MaxValueInFlight = tlv.SomeRecordT(rec)
	}
	if val, ok := typeMap[dc.HtlcMinimum.TlvType()]; ok && val == nil {
		var rec tlv.RecordT[tlv.TlvType4, MilliSatoshi]
		rec.Val = htlcMin.Val
		dc.HtlcMinimum = tlv.SomeRecordT(rec)
	}
	if val, ok := typeMap[dc.ChannelReserve.TlvType()]; ok && val == nil {
		var rec tlv.RecordT[tlv.TlvType6, tlv.BigSizeT[btcutil.Amount]]
		rec.Val = reserve.Val
		dc.ChannelReserve = tlv.SomeRecordT(rec)
	}
	if val, ok := typeMap[dc.CsvDelay.TlvType()]; ok && val == nil {
		dc.CsvDelay = tlv.SomeRecordT(csvDelay)
	}
	if val, ok := typeMap[dc.MaxAcceptedHTLCs.TlvType()]; ok && val == nil {
		dc.MaxAcceptedHTLCs = tlv.SomeRecordT(maxHtlcs)
	}
	if val, ok := typeMap[dc.ChannelType.TlvType()]; ok && val == nil {
		dc.ChannelType = tlv.SomeRecordT(chanType)
	}

	if len(tlvRecords) != 0 {
		dc.ExtraData = tlvRecords
	}

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynCommit on the wire.
//
// This is part of the lnwire.Message interface.
func (dc *DynCommit) MsgType() MessageType {
	return MsgDynCommit
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (dc *DynCommit) SerializedSize() (uint32, error) {
	return MessageSerializedSize(dc)
}
