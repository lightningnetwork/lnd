package record

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// AverageDummyHopPayloadSize is the size of a standard blinded path dummy hop
// payload. In most cases, this is larger than the other payload types and so
// to make sure that a sender cannot use this fact to know if a dummy hop is
// present or not, we'll make sure to always pad all payloads to at least this
// size.
const AverageDummyHopPayloadSize = 51

// BlindedRouteData contains the information that is included in a blinded
// route encrypted data blob that is created by the recipient to provide
// forwarding information.
type BlindedRouteData struct {
	// Padding is an optional set of bytes that a recipient can use to pad
	// the data so that the encrypted recipient data blobs are all the same
	// length.
	Padding tlv.OptionalRecordT[tlv.TlvType1, []byte]

	// ShortChannelID is the channel ID of the next hop.
	ShortChannelID tlv.OptionalRecordT[tlv.TlvType2, lnwire.ShortChannelID]

	// NextNodeID is the node ID of the next node on the path. In the
	// context of blinded path payments, this is used to indicate the
	// presence of dummy hops that need to be peeled from the onion.
	NextNodeID tlv.OptionalRecordT[tlv.TlvType4, *btcec.PublicKey]

	// PathID is a secret set of bytes that the blinded path creator will
	// set so that they can check the value on decryption to ensure that the
	// path they created was used for the intended purpose.
	PathID tlv.OptionalRecordT[tlv.TlvType6, []byte]

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

// NewFinalHopBlindedRouteData creates the data that's provided for the final
// hop in a blinded route.
func NewFinalHopBlindedRouteData(constraints *PaymentConstraints,
	pathID []byte) *BlindedRouteData {

	var data BlindedRouteData
	if pathID != nil {
		data.PathID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType6](pathID),
		)
	}

	if constraints != nil {
		data.Constraints = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](*constraints))
	}

	return &data
}

// NewDummyHopRouteData creates the data that's provided for any hop preceding
// a dummy hop. The presence of such a payload indicates to the reader that
// they are the intended recipient and should peel the remainder of the onion.
func NewDummyHopRouteData(ourPubKey *btcec.PublicKey,
	relayInfo PaymentRelayInfo,
	constraints PaymentConstraints) *BlindedRouteData {

	return &BlindedRouteData{
		NextNodeID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType4](ourPubKey),
		),
		RelayInfo: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType10](relayInfo),
		),
		Constraints: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](constraints),
		),
	}
}

// DecodeBlindedRouteData decodes the data provided within a blinded route.
func DecodeBlindedRouteData(r io.Reader) (*BlindedRouteData, error) {
	var (
		d BlindedRouteData

		padding          = d.Padding.Zero()
		scid             = d.ShortChannelID.Zero()
		nextNodeID       = d.NextNodeID.Zero()
		pathID           = d.PathID.Zero()
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
		&padding, &scid, &nextNodeID, &pathID, &blindingOverride,
		&relayInfo, &constraints, &features,
	)
	if err != nil {
		return nil, err
	}

	val, ok := typeMap[d.Padding.TlvType()]
	if ok && val == nil {
		d.Padding = tlv.SomeRecordT(padding)
	}

	if val, ok := typeMap[d.ShortChannelID.TlvType()]; ok && val == nil {
		d.ShortChannelID = tlv.SomeRecordT(scid)
	}

	if val, ok := typeMap[d.NextNodeID.TlvType()]; ok && val == nil {
		d.NextNodeID = tlv.SomeRecordT(nextNodeID)
	}

	if val, ok := typeMap[d.PathID.TlvType()]; ok && val == nil {
		d.PathID = tlv.SomeRecordT(pathID)
	}

	val, ok = typeMap[d.NextBlindingOverride.TlvType()]
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

	data.Padding.WhenSome(func(p tlv.RecordT[tlv.TlvType1, []byte]) {
		recordProducers = append(recordProducers, &p)
	})

	data.ShortChannelID.WhenSome(func(scid tlv.RecordT[tlv.TlvType2,
		lnwire.ShortChannelID]) {

		recordProducers = append(recordProducers, &scid)
	})

	data.NextNodeID.WhenSome(func(f tlv.RecordT[tlv.TlvType4,
		*btcec.PublicKey]) {

		recordProducers = append(recordProducers, &f)
	})

	data.PathID.WhenSome(func(pathID tlv.RecordT[tlv.TlvType6, []byte]) {
		recordProducers = append(recordProducers, &pathID)
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

// PadBy adds "n" padding bytes to the BlindedRouteData using the Padding field.
// Callers should be aware that the total payload size will change by more than
// "n" since the "n" bytes will be prefixed by BigSize type and length fields.
// Callers may need to call PadBy iteratively until each encrypted data packet
// is the same size and so each call will overwrite the Padding record.
// Note that calling PadBy with an n value of 0 will still result in a zero
// length TLV entry being added.
func (b *BlindedRouteData) PadBy(n int) {
	b.Padding = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType1](make([]byte, n)),
	)
}

// PaymentRelayInfo describes the relay policy for a blinded path.
type PaymentRelayInfo struct {
	// CltvExpiryDelta is the expiry delta for the payment.
	CltvExpiryDelta uint16

	// FeeRate is the fee rate that will be charged per millionth of a
	// satoshi.
	FeeRate uint32

	// BaseFee is the per-htlc fee charged in milli-satoshis.
	BaseFee lnwire.MilliSatoshi
}

// Record creates a tlv.Record that encodes the payment relay (type 10) type for
// an encrypted blob payload.
func (i *PaymentRelayInfo) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		10, &i, func() uint64 {
			// uint16 + uint32 + tuint32
			return 2 + 4 + tlv.SizeTUint32(uint32(i.BaseFee))
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

		baseFee := uint32(relayInfo.BaseFee)

		// We can safely reuse buf here because we overwrite its
		// contents.
		return tlv.ETUint32(w, &baseFee, buf)
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

		var baseFee uint32
		err = tlv.DTUint32(b, &baseFee, buf, l-6)
		if err != nil {
			return err
		}

		relayInfo.BaseFee = lnwire.MilliSatoshi(baseFee)

		return nil
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
