package record

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// Onion Routing Packet types.

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

	// Onion Message Packet types.

	// ReplyPathType is the type used in the onion message to indicate the
	// blinded path to be used for replies.
	ReplyPathType tlv.Type = 2

	// EncryptedRecipientDataType is the type used in the onion message to
	// include encrypted data in the onion for use in blinded paths.
	EncryptedRecipientDataType tlv.Type = 4

	// InvoiceRequestType is the type used in the onion message to include
	// invoice requests.
	InvoiceRequestType tlv.Type = 64

	// InvoiceType is the type used in the onion message to include
	// invoices.
	InvoiceType tlv.Type = 66

	// InvoiceErrorType is the type used in the onion message to include
	// invoice errors.
	InvoiceErrorType tlv.Type = 68
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

// NewEncryptedRecipientDataRecord creates a tlv.Record that encodes the
// encrypted_data (type 4) record for an onion message payload.
func NewEncryptedRecipientDataRecord(data *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(EncryptedRecipientDataType, data)
}

// NewReplyPathRecord creates a tlv.Record that encodes the reply_path (type 2)
// record for an onion message payload.
func NewReplyPathRecord(data *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(ReplyPathType, data)
}

// NewInvoiceRequestRecord creates a tlv.Record that encodes the
// invoice_request (type 64) record for an onion message payload.
func NewInvoiceRequestRecord(data *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(InvoiceRequestType, data)
}

// NewInvoiceRecord creates a tlv.Record that encodes the
// invoice (type 66) record for an onion message payload.
func NewInvoiceRecord(data *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(InvoiceType, data)
}

// NewInvoiceErrorRecord creates a tlv.Record that encodes the
// invoice_error (type 68) record for an onion message payload.
func NewInvoiceErrorRecord(data *[]byte) tlv.Record {
	return tlv.MakePrimitiveRecord(InvoiceErrorType, data)
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
