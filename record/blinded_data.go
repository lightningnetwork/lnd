package record

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// ShortChannelIDType is a record type for the outgoing channel short
	// ID.
	ShortChannelIDType tlv.Type = 2

	// NextNodeType is a record type for the unblinded next node ID.
	NextNodeType tlv.Type = 4

	// PaymentRelayType is the record type for a tlv containing fee and
	// cltv forwarding information.
	PaymentRelayType tlv.Type = 10

	// PaymentConstraintType is a tlv containing the constraints placed
	// on a forwarded payment.
	PaymentConstraintType tlv.Type = 12

	// FeatureVectorType is the record type for a tlv with the features
	// supported by the blinded hop.
	FeatureVectorType tlv.Type = 14
)

// BlindedRouteData contains the information that is included in a blinded
// route encrypted data blob.
type BlindedRouteData struct {
	// ShortChannelID is the channel ID of the next hop.
	ShortChannelID *lnwire.ShortChannelID

	// NextNodeID is the unblinded node ID of the next hop.
	NextNodeID *btcec.PublicKey

	// RelayInfo provides the relay parameters for the hop.
	RelayInfo *PaymentRelayInfo

	// Constraints provides the payment relay constraints for the hop.
	Constraints *PaymentConstraints

	// Features is the set of features the payment requires.
	Features *lnwire.FeatureVector
}

// DecodeBlindedRouteData decodes the data provided within a blinded route.
func DecodeBlindedRouteData(r io.Reader) (*BlindedRouteData, error) {
	var (
		routeData = &BlindedRouteData{
			RelayInfo:   &PaymentRelayInfo{},
			Constraints: &PaymentConstraints{},
			// We create a non-nil but empty set of features by
			// default, so that we don't need to worry about nil
			// values and can decode directly into the raw vector.
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(), lnwire.Features,
			),
		}

		shortID uint64
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(ShortChannelIDType, &shortID),
		tlv.MakePrimitiveRecord(NextNodeType, &routeData.NextNodeID),
		newPaymentRelayRecord(routeData.RelayInfo),
		newPaymentConstraintsRecord(routeData.Constraints),
		routeData.Features.Record(FeatureVectorType),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	tlvMap, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return nil, err
	}

	if _, ok := tlvMap[PaymentRelayType]; !ok {
		routeData.RelayInfo = nil
	}

	if _, ok := tlvMap[PaymentConstraintType]; !ok {
		routeData.Constraints = nil
	}

	if _, ok := tlvMap[ShortChannelIDType]; ok {
		shortID := lnwire.NewShortChanIDFromInt(shortID)
		routeData.ShortChannelID = &shortID
	}

	return routeData, nil
}

// EncodeBlindedRouteData encodes the blinded route data provided.
func EncodeBlindedRouteData(data *BlindedRouteData) ([]byte, error) {
	var (
		w       = new(bytes.Buffer)
		records []tlv.Record
	)

	if data.ShortChannelID != nil {
		shortID := data.ShortChannelID.ToUint64()
		shortIDRecord := tlv.MakePrimitiveRecord(
			ShortChannelIDType, &shortID,
		)

		records = append(records, shortIDRecord)
	}

	if data.NextNodeID != nil {
		nodeIDRecord := tlv.MakePrimitiveRecord(
			NextNodeType, &data.NextNodeID,
		)
		records = append(records, nodeIDRecord)
	}

	if data.RelayInfo != nil {
		relayRecord := newPaymentRelayRecord(data.RelayInfo)
		records = append(records, relayRecord)
	}

	if data.Constraints != nil {
		constraintsRecord := newPaymentConstraintsRecord(
			data.Constraints,
		)
		records = append(records, constraintsRecord)
	}

	if data.Features != nil && !data.Features.IsEmpty() {
		featuresRecord := data.Features.RawFeatureVector.Record(
			FeatureVectorType,
		)
		records = append(records, featuresRecord)
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	if err := stream.Encode(w); err != nil {
		return nil, err
	}

	return w.Bytes(), nil
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
func newPaymentRelayRecord(info *PaymentRelayInfo) tlv.Record {
	return tlv.MakeDynamicRecord(
		PaymentRelayType, &info, func() uint64 {
			// uint16 + uint32 + tuint32
			return 2 + 4 + tlv.SizeTUint32(info.BaseFee)
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

	return tlv.NewTypeForEncodingErr(val, "*hop.paymentRelayInfo")
}

func decodePaymentRelay(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if t, ok := val.(**PaymentRelayInfo); ok && l <= 10 {
		scratch := make([]byte, l)

		n, err := io.ReadFull(r, scratch)
		if err != nil {
			return err
		}

		// We expect at least 6 bytes, because we have q, bytes for
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

func newPaymentConstraintsRecord(constraints *PaymentConstraints) tlv.Record {
	return tlv.MakeDynamicRecord(
		PaymentConstraintType, &constraints, func() uint64 {
			// uint32 + tuint64.
			return 4 + tlv.SizeTUint64(uint64(
				constraints.HtlcMinimumMsat,
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
