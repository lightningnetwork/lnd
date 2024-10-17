package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DPDustLimitSatoshis is the TLV type number that identifies the record
	// for DynPropose.DustLimit.
	DPDustLimitSatoshis tlv.Type = 0

	// DPMaxHtlcValueInFlightMsat is the TLV type number that identifies the
	// record for DynPropose.MaxValueInFlight.
	DPMaxHtlcValueInFlightMsat tlv.Type = 2

	// DPHtlcMinimumMsat is the TLV type number that identifies the record
	// for DynPropose.HtlcMinimum.
	DPHtlcMinimumMsat tlv.Type = 4

	// DPChannelReserveSatoshis is the TLV type number that identifies the
	// for DynPropose.ChannelReserve.
	DPChannelReserveSatoshis tlv.Type = 6

	// DPToSelfDelay is the TLV type number that identifies the record for
	// DynPropose.CsvDelay.
	DPToSelfDelay tlv.Type = 8

	// DPMaxAcceptedHtlcs is the TLV type number that identifies the record
	// for DynPropose.MaxAcceptedHTLCs.
	DPMaxAcceptedHtlcs tlv.Type = 10

	// DPChannelType is the TLV type number that identifies the record for
	// DynPropose.ChannelType.
	DPChannelType tlv.Type = 12
)

// DynPropose is a message that is sent during a dynamic commitments negotiation
// process. It is sent by both parties to propose new channel parameters.
type DynPropose struct {
	// ChanID identifies the channel whose parameters we are trying to
	// re-negotiate.
	ChanID ChannelID

	// DustLimit, if not nil, proposes a change to the dust_limit_satoshis
	// for the sender's commitment transaction.
	DustLimit fn.Option[btcutil.Amount]

	// MaxValueInFlight, if not nil, proposes a change to the
	// max_htlc_value_in_flight_msat limit of the sender.
	MaxValueInFlight fn.Option[MilliSatoshi]

	// HtlcMinimum, if not nil, proposes a change to the htlc_minimum_msat
	// floor of the sender.
	HtlcMinimum fn.Option[MilliSatoshi]

	// ChannelReserve, if not nil, proposes a change to the
	// channel_reserve_satoshis requirement of the recipient.
	ChannelReserve fn.Option[btcutil.Amount]

	// CsvDelay, if not nil, proposes a change to the to_self_delay
	// requirement of the recipient.
	CsvDelay fn.Option[uint16]

	// MaxAcceptedHTLCs, if not nil, proposes a change to the
	// max_accepted_htlcs limit of the sender.
	MaxAcceptedHTLCs fn.Option[uint16]

	// ChannelType, if not nil, proposes a change to the channel_type
	// parameter.
	ChannelType fn.Option[ChannelType]

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

// Encode serializes the target DynPropose into the passed io.Writer.
// Serialization will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (dp *DynPropose) Encode(w *bytes.Buffer, _ uint32) error {
	var tlvRecords []tlv.Record
	dp.DustLimit.WhenSome(func(dl btcutil.Amount) {
		protoSats := uint64(dl)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPDustLimitSatoshis, &protoSats,
			),
		)
	})
	dp.MaxValueInFlight.WhenSome(func(max MilliSatoshi) {
		protoSats := uint64(max)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPMaxHtlcValueInFlightMsat, &protoSats,
			),
		)
	})
	dp.HtlcMinimum.WhenSome(func(min MilliSatoshi) {
		protoSats := uint64(min)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPHtlcMinimumMsat, &protoSats,
			),
		)
	})
	dp.ChannelReserve.WhenSome(func(min btcutil.Amount) {
		channelReserve := uint64(min)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPChannelReserveSatoshis, &channelReserve,
			),
		)
	})
	dp.CsvDelay.WhenSome(func(wait uint16) {
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPToSelfDelay, &wait,
			),
		)
	})
	dp.MaxAcceptedHTLCs.WhenSome(func(max uint16) {
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPMaxAcceptedHtlcs, &max,
			),
		)
	})
	dp.ChannelType.WhenSome(func(ty ChannelType) {
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
	dp.ExtraData = ExtraOpaqueData(extraBytesWriter.Bytes())

	if err := WriteChannelID(w, dp.ChanID); err != nil {
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
	var dustLimitScratch uint64
	dustLimit := tlv.MakePrimitiveRecord(
		DPDustLimitSatoshis, &dustLimitScratch,
	)

	var maxValueScratch uint64
	maxValue := tlv.MakePrimitiveRecord(
		DPMaxHtlcValueInFlightMsat, &maxValueScratch,
	)

	var htlcMinScratch uint64
	htlcMin := tlv.MakePrimitiveRecord(
		DPHtlcMinimumMsat, &htlcMinScratch,
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
		dustLimit, maxValue, htlcMin, reserve, csvDelay, maxHtlcs,
		chanType,
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
		dp.DustLimit = fn.Some(btcutil.Amount(dustLimitScratch))
	}
	if val, ok := typeMap[DPMaxHtlcValueInFlightMsat]; ok && val == nil {
		dp.MaxValueInFlight = fn.Some(MilliSatoshi(maxValueScratch))
	}
	if val, ok := typeMap[DPHtlcMinimumMsat]; ok && val == nil {
		dp.HtlcMinimum = fn.Some(MilliSatoshi(htlcMinScratch))
	}
	if val, ok := typeMap[DPChannelReserveSatoshis]; ok && val == nil {
		dp.ChannelReserve = fn.Some(btcutil.Amount(reserveScratch))
	}
	if val, ok := typeMap[DPToSelfDelay]; ok && val == nil {
		dp.CsvDelay = fn.Some(csvDelayScratch)
	}
	if val, ok := typeMap[DPMaxAcceptedHtlcs]; ok && val == nil {
		dp.MaxAcceptedHTLCs = fn.Some(maxHtlcsScratch)
	}
	if val, ok := typeMap[DPChannelType]; ok && val == nil {
		dp.ChannelType = fn.Some(chanTypeScratch)
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

// SerializeTlvData takes just the TLV data of DynPropose (which covers all of
// the parameters on deck for changing) and serializes just this component. The
// main purpose of this is to make it easier to validate the DynAck signature.
func (dp *DynPropose) SerializeTlvData() ([]byte, error) {
	var tlvRecords []tlv.Record
	dp.DustLimit.WhenSome(func(dl btcutil.Amount) {
		protoSats := uint64(dl)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPDustLimitSatoshis, &protoSats,
			),
		)
	})
	dp.MaxValueInFlight.WhenSome(func(max MilliSatoshi) {
		protoSats := uint64(max)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPMaxHtlcValueInFlightMsat, &protoSats,
			),
		)
	})
	dp.HtlcMinimum.WhenSome(func(min MilliSatoshi) {
		protoSats := uint64(min)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPHtlcMinimumMsat, &protoSats,
			),
		)
	})
	dp.ChannelReserve.WhenSome(func(min btcutil.Amount) {
		channelReserve := uint64(min)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPChannelReserveSatoshis, &channelReserve,
			),
		)
	})
	dp.CsvDelay.WhenSome(func(wait uint16) {
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPToSelfDelay, &wait,
			),
		)
	})
	dp.MaxAcceptedHTLCs.WhenSome(func(max uint16) {
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				DPMaxAcceptedHtlcs, &max,
			),
		)
	})
	dp.ChannelType.WhenSome(func(ty ChannelType) {
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
		return nil, err
	}

	var outBuf bytes.Buffer
	err = tlvStream.Encode(&outBuf)
	if err != nil {
		return nil, err
	}

	return outBuf.Bytes(), nil
}
