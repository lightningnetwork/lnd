package record

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// AmtOnionType is the type used in the onion to reference the amount to
	// send to the next hop.
	AmtOnionType tlv.Type = 2

	// LockTimeTLV is the type used in the onion to reference the CLTV
	// value that should be used for the next hop's HTLC.
	LockTimeOnionType tlv.Type = 4

	// NextHopOnionType is the type used in the onion to reference the ID
	// of the next hop.
	NextHopOnionType tlv.Type = 6

	// RouteBlindingEncryptedDataOnionType refers to an encrypted route
	// blinding TLV payload which contains information needed to forward
	// in the blinded portion of a route.
	RouteBlindingEncryptedDataOnionType tlv.Type = 10

	// BlindingPointOnionType is the type used in the top level onion
	// payload to reference an ephemeral public key which is used to
	// establish a shared secret between a processing node in the
	// blinded portion of a route and the node which built the blinded
	// route (usually the recipient). The shared secret can then be used
	// for the purpose of decrypting the route blinding payload.
	//
	// NOTE: The ephemeral blinding point is delivered in the onion payload
	// for the first hop ("introduction node") of a blinded route ONLY!
	// Other processing nodes in a blinded route receive their blinding
	// point via TLV extension in the UpdateAddHTLC message.
	BlindingPointOnionType tlv.Type = 12

	// MetadataOnionType is the type used in the onion for the payment
	// metadata.
	MetadataOnionType tlv.Type = 16

	// TotalAmountMsatOnionType is the type used in the onion for
	// the total value of the payment and is intended for use with
	// the final hop in a route only.
	// TODO(9/11/22): Determine if this is needed or if we can use the
	// similar sounding field available on MPP.
	TotalAmountMsatOnionType tlv.Type = 18
)

// NewAmtToFwdRecord creates a tlv.Record that encodes the amount_to_forward
// (type 2) for an onion payload.
func NewAmtToFwdRecord(amt *uint64) tlv.Record {
	return tlv.MakeDynamicRecord(
		AmtOnionType, amt, func() uint64 {
			return tlv.SizeTUint64(*amt)
		},
		tlv.ETUint64, tlv.DTUint64,
	)
}

// NewLockTimeRecord creates a tlv.Record that encodes the outgoing_cltv_value
// (type 4) for an onion payload.
func NewLockTimeRecord(lockTime *uint32) tlv.Record {
	return tlv.MakeDynamicRecord(
		LockTimeOnionType, lockTime, func() uint64 {
			return tlv.SizeTUint32(*lockTime)
		},
		tlv.ETUint32, tlv.DTUint32,
	)
}

// NewNextHopIDRecord creates a tlv.Record that encodes the short_channel_id
// (type 6) for an onion payload.
func NewNextHopIDRecord(cid *uint64) tlv.Record {
	return tlv.MakePrimitiveRecord(NextHopOnionType, cid)
}

// NewRouteBlindingEncryptedDataRecord creates a tlv.Record that encodes the
// encrypted_recipient_data (type 10) for an onion payload.
func NewRouteBlindingEncryptedDataRecord(data *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(RouteBlindingEncryptedDataOnionType, data)
}

// NewBlindingPointRecord creates a tlv.Record that encodes the blinding_point
// (type 12) for an onion payload.
func NewBlindingPointRecord(key **btcec.PublicKey) tlv.Record {
	return tlv.MakePrimitiveRecord(BlindingPointOnionType, key)
}

// NewMetadataRecord creates a tlv.Record that encodes the metadata (type 16)
// for an onion payload.
func NewMetadataRecord(metadata *[]byte) tlv.Record {
	return tlv.MakeDynamicRecord(
		MetadataOnionType, metadata,
		func() uint64 {
			return uint64(len(*metadata))
		},
		tlv.EVarBytes, tlv.DVarBytes,
	)
}

// NewTotalAmountMsatRecord creates a tlv.Record that encodes the
// total payment amount in millisatoshis, total_amount_msat,
// (type 18) for an onion payload.
func NewTotalAmountMsatRecord(amt *uint64) tlv.Record {
	return tlv.MakeDynamicRecord(
		TotalAmountMsatOnionType, amt, func() uint64 {
			return tlv.SizeTUint64(*amt)
		},
		tlv.ETUint64, tlv.DTUint64,
	)
}
