package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// DynPropose is a message that is sent during a dynamic commitments negotiation
// process. It is sent by both parties to propose new channel parameters.
type DynPropose struct {
	// ChanID identifies the channel whose parameters we are trying to
	// re-negotiate.
	ChanID ChannelID

	// DustLimit, if not nil, proposes a change to the dust_limit_satoshis
	// for the sender's commitment transaction.
	DustLimit tlv.OptionalRecordT[tlv.TlvType0, btcutil.Amount]

	// MaxValueInFlight, if not nil, proposes a change to the
	// max_htlc_value_in_flight_msat limit of the sender.
	MaxValueInFlight tlv.OptionalRecordT[tlv.TlvType2, MilliSatoshi]

	// HtlcMinimum, if not nil, proposes a change to the htlc_minimum_msat
	// floor of the sender.
	HtlcMinimum tlv.OptionalRecordT[tlv.TlvType4, MilliSatoshi]

	// ChannelReserve, if not nil, proposes a change to the
	// channel_reserve_satoshis requirement of the recipient.
	ChannelReserve tlv.OptionalRecordT[tlv.TlvType6, btcutil.Amount]

	// CsvDelay, if not nil, proposes a change to the to_self_delay
	// requirement of the recipient.
	CsvDelay tlv.OptionalRecordT[tlv.TlvType8, uint16]

	// MaxAcceptedHTLCs, if not nil, proposes a change to the
	// max_accepted_htlcs limit of the sender.
	MaxAcceptedHTLCs tlv.OptionalRecordT[tlv.TlvType10, uint16]

	// ChannelType, if not nil, proposes a change to the channel_type
	// parameter.
	ChannelType tlv.OptionalRecordT[tlv.TlvType12, ChannelType]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	//
	// NOTE: Since the fields in this structure are part of the TLV stream,
	// ExtraData will contain all TLV records _except_ the ones that are
	// present in earlier parts of this structure.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynPropose implements the lnwire.Message
// interface.
var _ Message = (*DynPropose)(nil)

// A compile time check to ensure DynPropose implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*DynPropose)(nil)

// Encode serializes the target DynPropose into the passed io.Writer.
// Serialization will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dp *DynPropose) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, dp.ChanID); err != nil {
		return err
	}

	producers := dynProposeRecords(dp)

	err := EncodeMessageExtraData(&dp.ExtraData, producers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, dp.ExtraData)
}

// Decode deserializes the serialized DynPropose stored in the passed io.Reader
// into the target DynPropose using the deserialization rules defined by the
// passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dp *DynPropose) Decode(r io.Reader, _ uint32) error {
	// Parse out the only required field.
	if err := ReadElements(r, &dp.ChanID); err != nil {
		return err
	}

	// Parse out TLV stream.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	// Prepare receiving buffers to be filled by TLV extraction.
	var dustLimit tlv.RecordT[tlv.TlvType0, uint64]
	var maxValue tlv.RecordT[tlv.TlvType2, uint64]
	var htlcMin tlv.RecordT[tlv.TlvType4, uint64]
	var reserve tlv.RecordT[tlv.TlvType6, uint64]
	csvDelay := dp.CsvDelay.Zero()
	maxHtlcs := dp.MaxAcceptedHTLCs.Zero()
	chanType := dp.ChannelType.Zero()

	typeMap, err := tlvRecords.ExtractRecords(
		&dustLimit, &maxValue, &htlcMin, &reserve, &csvDelay, &maxHtlcs,
		&chanType,
	)
	if err != nil {
		return err
	}

	// Check the results of the TLV Stream decoding and appropriately set
	// message fields.
	if val, ok := typeMap[dp.DustLimit.TlvType()]; ok && val == nil {
		var rec tlv.RecordT[tlv.TlvType0, btcutil.Amount]
		rec.Val = btcutil.Amount(dustLimit.Val)
		dp.DustLimit = tlv.SomeRecordT(rec)
	}
	if val, ok := typeMap[dp.MaxValueInFlight.TlvType()]; ok && val == nil {
		var rec tlv.RecordT[tlv.TlvType2, MilliSatoshi]
		rec.Val = MilliSatoshi(maxValue.Val)
		dp.MaxValueInFlight = tlv.SomeRecordT(rec)
	}
	if val, ok := typeMap[dp.HtlcMinimum.TlvType()]; ok && val == nil {
		var rec tlv.RecordT[tlv.TlvType4, MilliSatoshi]
		rec.Val = MilliSatoshi(htlcMin.Val)
		dp.HtlcMinimum = tlv.SomeRecordT(rec)
	}
	if val, ok := typeMap[dp.ChannelReserve.TlvType()]; ok && val == nil {
		var rec tlv.RecordT[tlv.TlvType6, btcutil.Amount]
		rec.Val = btcutil.Amount(reserve.Val)
		dp.ChannelReserve = tlv.SomeRecordT(rec)
	}
	if val, ok := typeMap[dp.CsvDelay.TlvType()]; ok && val == nil {
		dp.CsvDelay = tlv.SomeRecordT(csvDelay)
	}
	if val, ok := typeMap[dp.MaxAcceptedHTLCs.TlvType()]; ok && val == nil {
		dp.MaxAcceptedHTLCs = tlv.SomeRecordT(maxHtlcs)
	}
	if val, ok := typeMap[dp.ChannelType.TlvType()]; ok && val == nil {
		dp.ChannelType = tlv.SomeRecordT(chanType)
	}

	if len(tlvRecords) != 0 {
		dp.ExtraData = tlvRecords
	}

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynPropose on the wire.
//
// This is part of the lnwire.Message interface.
func (dp *DynPropose) MsgType() MessageType {
	return MsgDynPropose
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (dp *DynPropose) SerializedSize() (uint32, error) {
	return MessageSerializedSize(dp)
}

// SerializeTlvData takes just the TLV data of DynPropose (which covers all of
// the parameters on deck for changing) and serializes just this component. The
// main purpose of this is to make it easier to validate the DynAck signature.
func (dp *DynPropose) SerializeTlvData() ([]byte, error) {
	producers := dynProposeRecords(dp)

	var extra ExtraOpaqueData
	err := extra.PackRecords(producers...)
	if err != nil {
		return nil, err
	}

	return extra, nil
}

func dynProposeRecords(dp *DynPropose) []tlv.RecordProducer {
	recordProducers := make([]tlv.RecordProducer, 0, 7)

	dp.DustLimit.WhenSome(
		func(dl tlv.RecordT[tlv.TlvType0, btcutil.Amount]) {
			rec := tlv.NewPrimitiveRecord[tlv.TlvType0](
				uint64(dl.Val),
			)
			recordProducers = append(recordProducers, &rec)
		},
	)
	dp.MaxValueInFlight.WhenSome(
		func(max tlv.RecordT[tlv.TlvType2, MilliSatoshi]) {
			rec := tlv.NewPrimitiveRecord[tlv.TlvType2](
				uint64(max.Val),
			)
			recordProducers = append(recordProducers, &rec)
		},
	)
	dp.HtlcMinimum.WhenSome(
		func(min tlv.RecordT[tlv.TlvType4, MilliSatoshi]) {
			rec := tlv.NewPrimitiveRecord[tlv.TlvType4](
				uint64(min.Val),
			)
			recordProducers = append(recordProducers, &rec)
		},
	)
	dp.ChannelReserve.WhenSome(
		func(reserve tlv.RecordT[tlv.TlvType6, btcutil.Amount]) {
			rec := tlv.NewPrimitiveRecord[tlv.TlvType6](
				uint64(reserve.Val),
			)
			recordProducers = append(recordProducers, &rec)
		},
	)
	dp.CsvDelay.WhenSome(
		func(wait tlv.RecordT[tlv.TlvType8, uint16]) {
			recordProducers = append(recordProducers, &wait)
		},
	)
	dp.MaxAcceptedHTLCs.WhenSome(
		func(max tlv.RecordT[tlv.TlvType10, uint16]) {
			recordProducers = append(recordProducers, &max)
		},
	)
	dp.ChannelType.WhenSome(
		func(ty tlv.RecordT[tlv.TlvType12, ChannelType]) {
			recordProducers = append(recordProducers, &ty)
		},
	)

	return recordProducers
}
