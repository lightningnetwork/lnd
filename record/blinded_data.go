package record

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// BlindedRouteData contains the information that is included in a blinded
// route encrypted data blob that is created by the recipient to provide
// forwarding information.
type BlindedRouteData struct {
	// ShortChannelID is the channel ID of the next hop.
	ShortChannelID tlv.OptionalRecordT[tlv.TlvType2, lnwire.ShortChannelID]

	// NextBlindingOverride is a blinding point that should be switched
	// in for the next hop. This is used to combine two blinded paths into
	// one (which primarily is used in onion messaging, but in theory
	// could be used for payments as well).
	NextBlindingOverride tlv.OptionalRecordT[tlv.TlvType8, *btcec.PublicKey]

	// RelayInfo provides the relay parameters for the hop.
	RelayInfo tlv.OptionalRecordT[tlv.TlvType10, PaymentRelayInfo]

	// Constraints provides the payment relay constraints for the hop.
	Constraints tlv.OptionalRecordT[tlv.TlvType12, PaymentConstraints]

	// Features is the set of features the payment requires.
	Features tlv.OptionalRecordT[tlv.TlvType14, lnwire.FeatureVector]
}

// NewNonFinalBlindedRouteData creates the data that's provided for hops within
// a blinded route.
func NewNonFinalBlindedRouteData(chanID lnwire.ShortChannelID,
	blindingOverride *btcec.PublicKey, relayInfo PaymentRelayInfo,
	constraints *PaymentConstraints,
	features *lnwire.FeatureVector) *BlindedRouteData {

	info := &BlindedRouteData{
		ShortChannelID: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](chanID),
		),
		RelayInfo: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType10](relayInfo),
		),
	}

	if blindingOverride != nil {
		info.NextBlindingOverride = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType8](blindingOverride))
	}

	if constraints != nil {
		info.Constraints = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](*constraints))
	}

	if features != nil {
		info.Features = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType14](*features),
		)
	}

	return info
}

// DecodeBlindedRouteData decodes the data provided within a blinded route.
func DecodeBlindedRouteData(r io.Reader) (*BlindedRouteData, error) {
	var (
		d BlindedRouteData

		scid             = d.ShortChannelID.Zero()
		blindingOverride = d.NextBlindingOverride.Zero()
		relayInfo        = d.RelayInfo.Zero()
		constraints      = d.Constraints.Zero()
		features         = d.Features.Zero()
	)

	var tlvRecords lnwire.ExtraOpaqueData
	if err := lnwire.ReadElements(r, &tlvRecords); err != nil {
		return nil, err
	}

	typeMap, err := tlvRecords.ExtractRecords(
		&scid, &blindingOverride, &relayInfo, &constraints, &features,
	)
	if err != nil {
		return nil, err
	}

	if val, ok := typeMap[d.ShortChannelID.TlvType()]; ok && val == nil {
		d.ShortChannelID = tlv.SomeRecordT(scid)
	}

	val, ok := typeMap[d.NextBlindingOverride.TlvType()]
	if ok && val == nil {
		d.NextBlindingOverride = tlv.SomeRecordT(blindingOverride)
	}

	if val, ok := typeMap[d.RelayInfo.TlvType()]; ok && val == nil {
		d.RelayInfo = tlv.SomeRecordT(relayInfo)
	}

	if val, ok := typeMap[d.Constraints.TlvType()]; ok && val == nil {
		d.Constraints = tlv.SomeRecordT(constraints)
	}

	if val, ok := typeMap[d.Features.TlvType()]; ok && val == nil {
		d.Features = tlv.SomeRecordT(features)
	}

	return &d, nil
}

// EncodeBlindedRouteData encodes the blinded route data provided.
func EncodeBlindedRouteData(data *BlindedRouteData) ([]byte, error) {
	var (
		e               lnwire.ExtraOpaqueData
		recordProducers = make([]tlv.RecordProducer, 0, 5)
	)

	data.ShortChannelID.WhenSome(func(scid tlv.RecordT[tlv.TlvType2,
		lnwire.ShortChannelID]) {

		recordProducers = append(recordProducers, &scid)
	})

	data.NextBlindingOverride.WhenSome(func(pk tlv.RecordT[tlv.TlvType8,
		*btcec.PublicKey]) {

		recordProducers = append(recordProducers, &pk)
	})

	data.RelayInfo.WhenSome(func(r tlv.RecordT[tlv.TlvType10,
		PaymentRelayInfo]) {

		recordProducers = append(recordProducers, &r)
	})

	data.Constraints.WhenSome(func(cs tlv.RecordT[tlv.TlvType12,
		PaymentConstraints]) {

		recordProducers = append(recordProducers, &cs)
	})

	data.Features.WhenSome(func(f tlv.RecordT[tlv.TlvType14,
		lnwire.FeatureVector]) {

		recordProducers = append(recordProducers, &f)
	})

	if err := e.PackRecords(recordProducers...); err != nil {
		return nil, err
	}

	return e[:], nil
}

// PaymentRelayInfo describes the relay policy for a blinded path.
type PaymentRelayInfo struct {
	// CltvExpiryDelta is the expiry delta for the payment.
	CltvExpiryDelta uint16

	// FeeRate is the fee rate that will be charged per millionth of a
	// satoshi.
	FeeRate uint32

	// BaseFee is the per-htlc fee charged.
	BaseFee uint32
}

// newPaymentRelayRecord creates a tlv.Record that encodes the payment relay
// (type 10) type for an encrypted blob payload.
func (i *PaymentRelayInfo) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		10, &i, func() uint64 {
			// uint16 + uint32 + tuint32
			return 2 + 4 + tlv.SizeTUint32(i.BaseFee)
		}, encodePaymentRelay, decodePaymentRelay,
	)
}

func encodePaymentRelay(w io.Writer, val interface{}, buf *[8]byte) error {
	if t, ok := val.(**PaymentRelayInfo); ok {
		relayInfo := *t

		// Just write our first 6 bytes directly.
		binary.BigEndian.PutUint16(buf[:2], relayInfo.CltvExpiryDelta)
		binary.BigEndian.PutUint32(buf[2:6], relayInfo.FeeRate)
		if _, err := w.Write(buf[0:6]); err != nil {
			return err
		}

		// We can safely reuse buf here because we overwrite its
		// contents.
		return tlv.ETUint32(w, &relayInfo.BaseFee, buf)
	}

	return tlv.NewTypeForEncodingErr(val, "**hop.PaymentRelayInfo")
}

func decodePaymentRelay(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if t, ok := val.(**PaymentRelayInfo); ok && l <= 10 {
		scratch := make([]byte, l)

		n, err := io.ReadFull(r, scratch)
		if err != nil {
			return err
		}

		// We expect at least 6 bytes, because we have 2 bytes for
		// cltv delta and 4 bytes for fee rate.
		if n < 6 {
			return tlv.NewTypeForDecodingErr(val,
				"*hop.paymentRelayInfo", uint64(n), 6)
		}

		relayInfo := *t

		relayInfo.CltvExpiryDelta = binary.BigEndian.Uint16(
			scratch[0:2],
		)
		relayInfo.FeeRate = binary.BigEndian.Uint32(scratch[2:6])

		// To be able to re-use the DTUint32 function we create a
		// buffer with just the bytes holding the variable length u32.
		// If the base fee is zero, this will be an empty buffer, which
		// is okay.
		b := bytes.NewBuffer(scratch[6:])

		return tlv.DTUint32(b, &relayInfo.BaseFee, buf, l-6)
	}

	return tlv.NewTypeForDecodingErr(val, "*hop.paymentRelayInfo", l, 10)
}

// PaymentConstraints is a set of restrictions on a payment.
type PaymentConstraints struct {
	// MaxCltvExpiry is the maximum expiry height for the payment.
	MaxCltvExpiry uint32

	// HtlcMinimumMsat is the minimum htlc size for the payment.
	HtlcMinimumMsat lnwire.MilliSatoshi
}

func (p *PaymentConstraints) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		12, &p, func() uint64 {
			// uint32 + tuint64.
			return 4 + tlv.SizeTUint64(uint64(
				p.HtlcMinimumMsat,
			))
		},
		encodePaymentConstraints, decodePaymentConstraints,
	)
}

func encodePaymentConstraints(w io.Writer, val interface{},
	buf *[8]byte) error {

	if c, ok := val.(**PaymentConstraints); ok {
		constraints := *c

		binary.BigEndian.PutUint32(buf[:4], constraints.MaxCltvExpiry)
		if _, err := w.Write(buf[:4]); err != nil {
			return err
		}

		// We can safely re-use buf here because we overwrite its
		// contents.
		htlcMsat := uint64(constraints.HtlcMinimumMsat)

		return tlv.ETUint64(w, &htlcMsat, buf)
	}

	return tlv.NewTypeForEncodingErr(val, "**PaymentConstraints")
}

func decodePaymentConstraints(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if c, ok := val.(**PaymentConstraints); ok && l <= 12 {
		scratch := make([]byte, l)

		n, err := io.ReadFull(r, scratch)
		if err != nil {
			return err
		}

		// We expect at least 4 bytes for our uint32.
		if n < 4 {
			return tlv.NewTypeForDecodingErr(val,
				"*paymentConstraints", uint64(n), 4)
		}

		payConstraints := *c

		payConstraints.MaxCltvExpiry = binary.BigEndian.Uint32(
			scratch[:4],
		)

		// This could be empty if our minimum is zero, that's okay.
		var (
			b       = bytes.NewBuffer(scratch[4:])
			minHtlc uint64
		)

		err = tlv.DTUint64(b, &minHtlc, buf, l-4)
		if err != nil {
			return err
		}
		payConstraints.HtlcMinimumMsat = lnwire.MilliSatoshi(minHtlc)

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "**PaymentConstraints", l, l)
}
