package record

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (

	// PaddingOnionType is used in a route blinding TLV payload
	// to ensure that payloads are the same length across all
	// hops in a blinded route.
	PaddingOnionType tlv.Type = 1

	// NextHopOnionType is used in a route blinding TLV payload
	// to provide the short channel ID of the next hop.
	BlindedNextHopOnionType tlv.Type = 2

	// NextNodeIDOnionType is used in a route blinding TLV payload
	// to provide the persistent node ID of the next hop.
	NextNodeIDOnionType tlv.Type = 4

	// PathIDOnionType is an optional field recipients can use
	// to verify that a blinded route was used in the proper context.
	PathIDOnionType tlv.Type = 6

	// BlindingOverrideOnionType is field which can be used to
	// concatenate several distinct blinded routes.
	BlindingOverrideOnionType tlv.Type = 8

	// PaymentRelayOnionType specifies the information
	// necessary to compute the amount and timelock
	// which a blinded hop must use to forward the onion.
	PaymentRelayOnionType tlv.Type = 10

	// PaymentContraintsOnionType specifies a set of constraints
	// we are asked to honor during forwarding in the blinded
	// portion of a route. The constraints are chosen by the
	// route blinder in order to limit an adversary's ability to
	// unblind nodes within the route.
	//
	// NOTE: These are additional constraints on forwarded payments
	// above and beyond our usual forwarding policy.
	PaymentConstraintsOnionType tlv.Type = 12

	// AllowedFeaturesOnionType specifies the features a sender
	// is permitted to use when paying to a blinded route.
	AllowedFeaturesOnionType tlv.Type = 14
)

// NewPaddingRecord creates a tlv.Record that encodes the padding
// (type 1) for a route blinding payload.
func NewPaddingRecord(padding *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(PaddingOnionType, padding)
}

// NewUnblindedNextHopRecord creates a tlv.Record that encodes the short_channel_id
// (type 2) for a route blinding payload.
func NewBlindedNextHopRecord(cid *uint64) tlv.Record {
	return tlv.MakePrimitiveRecord(BlindedNextHopOnionType, cid)
}

// NewNextNodeIDRecord creates a tlv.Record that encodes the next_node_id
// (type 4) for a route blinding payload.
func NewNextNodeIDRecord(nodeID **btcec.PublicKey) tlv.Record {
	return tlv.MakePrimitiveRecord(NextNodeIDOnionType, nodeID)
}

// NewPathIDRecord creates a tlv.Record that encodes the path_id
// (type 6) for a route blinding payload.
func NewPathIDRecord(pathID *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(PathIDOnionType, pathID)
}

// NewBlindingOverrideRecord creates a tlv.Record that encodes the next_blinding_override
// (type 8) for a route blinding payload.
func NewBlindingOverrideRecord(point **btcec.PublicKey) tlv.Record {
	return tlv.MakePrimitiveRecord(BlindingOverrideOnionType, point)
}

// PaymentRelay is a tlv.Record which defines the payment_relay
// (type 10) for a route blinding payload.
type PaymentRelay struct {
	CltvExpiryDelta uint16
	FeeRate         uint32
	// NOTE(8/6/22): Is this in satoshis or msat?
	// This has implications for fee computation.
	BaseFee uint32
}

// satisfies the tlv.RecordProducer interface
// TODO(9/7/22): check length functions.
func (p *PaymentRelay) Record() tlv.Record {
	return tlv.MakeDynamicRecord(PaymentRelayOnionType,
		p, func() uint64 { return uint64(2 + 4 + 4) },
		paymentRelayEncoder, paymentRelayDecoder,
	)
}

func paymentRelayEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*PaymentRelay); ok {
		if err := tlv.EUint32(w, &v.BaseFee, buf); err != nil {
			return err
		}

		if err := tlv.EUint32(w, &v.FeeRate, buf); err != nil {
			return err
		}

		if err := tlv.EUint16(w, &v.CltvExpiryDelta, buf); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "routeblinding.PaymentRelay")
}

func paymentRelayDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*PaymentRelay); ok {
		if err := tlv.DUint32(r, &v.BaseFee, buf, 4); err != nil {
			return err
		}

		if err := tlv.DUint32(r, &v.FeeRate, buf, 4); err != nil {
			return err
		}

		if err := tlv.DUint16(r, &v.CltvExpiryDelta, buf, 2); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "routeblinding.PaymentRelay", l, 10)
}

// PaymentConstraints is a tlv.Record which defines the payment_constraints
// (type 12) for a route blinding payload.
type PaymentConstraints struct {
	MaxCltvExpiryDelta uint32
	HtlcMinimumMsat    uint64
	AllowedFeatures    []byte
}

func (p *PaymentConstraints) Record() tlv.Record {
	return tlv.MakeDynamicRecord(PaymentConstraintsOnionType,
		p, func() uint64 { return uint64(4 + 8 + len(p.AllowedFeatures)) },
		paymentConstraintsEncoder, paymentConstraintsDecoder,
	)
}

func paymentConstraintsEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*PaymentConstraints); ok {
		if err := tlv.EUint32(w, &v.MaxCltvExpiryDelta, buf); err != nil {
			return err
		}

		if err := tlv.EUint64(w, &v.HtlcMinimumMsat, buf); err != nil {
			return err
		}

		if err := tlv.EVarBytes(w, &v.AllowedFeatures, buf); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "routeblinding.PaymentConstraints")
}

func paymentConstraintsDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*PaymentConstraints); ok {
		if err := tlv.DUint32(r, &v.MaxCltvExpiryDelta, buf, 4); err != nil {
			return err
		}

		if err := tlv.DUint64(r, &v.HtlcMinimumMsat, buf, 8); err != nil {
			return err
		}

		if err := tlv.DVarBytes(r, &v.AllowedFeatures, buf, 0); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "routeblinding.PaymentRelay", l, 10)
}

// NewAllowedFeaturesRecord creates a tlv.Record that encodes the
// features permitted for use during forwarding inside a blinded route
// allowed features (type 14) for route blinding payload.
func NewAllowedFeaturesRecord(features *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(AllowedFeaturesOnionType, features)
}
