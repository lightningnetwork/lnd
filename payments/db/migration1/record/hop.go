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

	// EncryptedDataOnionType is the type used to include encrypted data
	// provided by the receiver in the onion for use in blinded paths.
	EncryptedDataOnionType tlv.Type = 10

	// BlindingPointOnionType is the type used to include receiver provided
	// ephemeral keys in the onion that are used in blinded paths.
	BlindingPointOnionType tlv.Type = 12

	// MetadataOnionType is the type used in the onion for the payment
	// metadata.
	MetadataOnionType tlv.Type = 16

	// TotalAmtMsatBlindedType is the type used in the onion for the total
	// amount field that is included in the final hop for blinded payments.
	TotalAmtMsatBlindedType tlv.Type = 18
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

// NewEncryptedDataRecord creates a tlv.Record that encodes the encrypted_data
// (type 10) record for an onion payload.
func NewEncryptedDataRecord(data *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(EncryptedDataOnionType, data)
}

// NewBlindingPointRecord creates a tlv.Record that encodes the blinding_point
// (type 12) record for an onion payload.
func NewBlindingPointRecord(point **btcec.PublicKey) tlv.Record {
	return tlv.MakePrimitiveRecord(BlindingPointOnionType, point)
}

// NewMetadataRecord creates a tlv.Record that encodes the metadata (type 10)
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

// NewTotalAmtMsatBlinded creates a tlv.Record that encodes the
// total_amount_msat for the final an onion payload within a blinded route.
func NewTotalAmtMsatBlinded(amt *uint64) tlv.Record {
	return tlv.MakeDynamicRecord(
		TotalAmtMsatBlindedType, amt, func() uint64 {
			return tlv.SizeTUint64(*amt)
		},
		tlv.ETUint64, tlv.DTUint64,
	)
}
