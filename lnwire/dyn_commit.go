package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
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

// A compile time check to ensure DynAck implements the lnwire.Message
// interface.
var _ Message = (*DynCommit)(nil)

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

	var tlvRecords []tlv.Record
	dc.DustLimit.WhenSome(func(dl btcutil.Amount) {
		protoSats := uint64(dl)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPDustLimitSatoshis, &protoSats,
			),
		)
	})
	dc.MaxValueInFlight.WhenSome(func(max MilliSatoshi) {
		protoSats := uint64(max)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPMaxHtlcValueInFlightMsat, &protoSats,
			),
		)
	})
	dc.ChannelReserve.WhenSome(func(min btcutil.Amount) {
		channelReserve := uint64(min)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPChannelReserveSatoshis, &channelReserve,
			),
		)
	})
	dc.CsvDelay.WhenSome(func(wait uint16) {
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPToSelfDelay, &wait,
			),
		)
	})
	dc.MaxAcceptedHTLCs.WhenSome(func(max uint16) {
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPMaxAcceptedHtlcs, &max,
			),
		)
	})
	dc.ChannelType.WhenSome(func(ty ChannelType) {
		tlvRecords = append(
			tlvRecords, tlv.MakeDynamicRecord(
				DPChannelType, &ty,
				ty.featureBitLen,
				channelTypeEncoder, channelTypeDecoder,
			),
		)
	})
	tlv.SortRecords(tlvRecords)

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	var extraBytesWriter bytes.Buffer
	if err := tlvStream.Encode(&extraBytesWriter); err != nil {
		return err
	}

	dc.ExtraData = ExtraOpaqueData(extraBytesWriter.Bytes())

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
	var dustLimitScratch uint64
	dustLimit := tlv.MakePrimitiveRecord(
		DPDustLimitSatoshis, &dustLimitScratch,
	)

	var maxValueScratch uint64
	maxValue := tlv.MakePrimitiveRecord(
		DPMaxHtlcValueInFlightMsat, &maxValueScratch,
	)

	var reserveScratch uint64
	reserve := tlv.MakePrimitiveRecord(
		DPChannelReserveSatoshis, &reserveScratch,
	)

	var csvDelayScratch uint16
	csvDelay := tlv.MakePrimitiveRecord(DPToSelfDelay, &csvDelayScratch)

	var maxHtlcsScratch uint16
	maxHtlcs := tlv.MakePrimitiveRecord(
		DPMaxAcceptedHtlcs, &maxHtlcsScratch,
	)

	var chanTypeScratch ChannelType
	chanType := tlv.MakeDynamicRecord(
		DPChannelType, &chanTypeScratch, chanTypeScratch.featureBitLen,
		channelTypeEncoder, channelTypeDecoder,
	)

	// Create set of Records to read TLV bytestream into.
	records := []tlv.Record{
		dustLimit, maxValue, reserve, csvDelay, maxHtlcs, chanType,
	}
	tlv.SortRecords(records)

	// Read TLV stream into record set.
	extraBytesReader := bytes.NewReader(tlvRecords)
	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}
	typeMap, err := tlvStream.DecodeWithParsedTypesP2P(extraBytesReader)
	if err != nil {
		return err
	}

	// Check the results of the TLV Stream decoding and appropriately set
	// message fields.
	if val, ok := typeMap[DPDustLimitSatoshis]; ok && val == nil {
		dc.DustLimit = fn.Some(btcutil.Amount(dustLimitScratch))
	}
	if val, ok := typeMap[DPMaxHtlcValueInFlightMsat]; ok && val == nil {
		dc.MaxValueInFlight = fn.Some(MilliSatoshi(maxValueScratch))
	}
	if val, ok := typeMap[DPChannelReserveSatoshis]; ok && val == nil {
		dc.ChannelReserve = fn.Some(btcutil.Amount(reserveScratch))
	}
	if val, ok := typeMap[DPToSelfDelay]; ok && val == nil {
		dc.CsvDelay = fn.Some(csvDelayScratch)
	}
	if val, ok := typeMap[DPMaxAcceptedHtlcs]; ok && val == nil {
		dc.MaxAcceptedHTLCs = fn.Some(maxHtlcsScratch)
	}
	if val, ok := typeMap[DPChannelType]; ok && val == nil {
		dc.ChannelType = fn.Some(chanTypeScratch)
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
