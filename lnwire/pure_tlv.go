package lnwire

import (
	"bytes"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// pureTLVUnsignedRangeOneStart defines the start of the first unsigned
	// TLV range used for pure TLV messages. The range is inclusive of this
	// number.
	pureTLVUnsignedRangeOneStart = 160

	// pureTLVSignedSecondRangeStart defines the start of the second signed
	// TLV range used for pure TLV messages. The range is inclusive of this
	// number. Note that the first range is the inclusive range of 0-159.
	pureTLVSignedSecondRangeStart = 1000000000

	// pureTLVUnsignedRangeTwoStart defines the start of the second unsigned
	// TLV range used for pure TLV message.
	pureTLVUnsignedRangeTwoStart = 3000000000
)

// PureTLVMessage describes an LN message that is a pure TLV stream. If the
// message includes a signature, it will sign all the TLV records in the
// inclusive ranges: 0 to 159 and 1000000000 to 2999999999.
type PureTLVMessage interface {
	// AllRecords returns all the TLV records for the message. This will
	// include all the records we know about along with any that we don't
	// know about but that fall in the signed TLV range.
	AllRecords() []tlv.Record
}

// EncodePureTLVMessage encodes the given PureTLVMessage to the given buffer.
func EncodePureTLVMessage(msg PureTLVMessage, buf *bytes.Buffer) error {
	return EncodeRecordsTo(buf, msg.AllRecords())
}

// SerialiseFieldsToSign serialises all the records from the given
// PureTLVMessage that fall within the signed TLV range.
func SerialiseFieldsToSign(msg PureTLVMessage) ([]byte, error) {
	// Filter out all the fields not in the signed ranges.
	var signedRecords []tlv.Record
	for _, record := range msg.AllRecords() {
		if InUnsignedRange(record.Type()) {
			continue
		}

		signedRecords = append(signedRecords, record)
	}

	var buf bytes.Buffer
	if err := EncodeRecordsTo(&buf, signedRecords); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// InUnsignedRange returns true if the given TLV type falls outside the TLV
// ranges that the signature of a pure TLV message will cover.
func InUnsignedRange(t tlv.Type) bool {
	return (t >= pureTLVUnsignedRangeOneStart &&
		t < pureTLVSignedSecondRangeStart) ||
		t >= pureTLVUnsignedRangeTwoStart
}

// ExtraSignedFields is a type that stores a map from TLV types in the signed
// range (for PureMessages) to their corresponding serialised values. This type
// can be used to keep around data that we don't yet understand but that we need
// for re-composing the wire message since the signature covers these fields.
type ExtraSignedFields map[uint64][]byte

// ExtraSignedFieldsFromTypeMap is a helper that can be used alongside calls to
// the tlv.Stream DecodeWithParsedTypesP2P or DecodeWithParsedTypes methods to
// extract the tlv type and value pairs in the defined PureTLVMessage signed
// range which we have not handled with any of our defined Records. These
// methods will return a tlv.TypeMap containing the records that were extracted
// from an io.Reader. If the record was know and handled by a defined record,
// then the value accompanying the record's type in the map will be nil.
// Otherwise, if the record was unhandled, it will be non-nil.
func ExtraSignedFieldsFromTypeMap(m tlv.TypeMap) ExtraSignedFields {
	extraFields := make(ExtraSignedFields)
	for t, v := range m {
		// If the value in the type map is nil, then it indicates that
		// we know this type, and it was handled by one of the records
		// we passed to the decode function vai the TLV stream.
		if v == nil {
			continue
		}

		// No need to keep this field if it is unknown to us and is not
		// in the sign range.
		if InUnsignedRange(t) {
			continue
		}

		// Otherwise, this is an un-handled type, so we keep track of
		// it for signature validation and re-encoding later on.
		extraFields[uint64(t)] = v
	}

	return extraFields
}
