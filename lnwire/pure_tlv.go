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
// message includes a signature, the signature covers a subset of the records,
// which subset is determined by the protocol's signed/unsigned range (see
// SerialiseFieldsToSignFn).
type PureTLVMessage interface {
	// AllRecords returns all the TLV records for the message, including
	// both records we know about and unknown records that we preserve.
	AllRecords() []tlv.Record
}

// EncodePureTLVMessage encodes the given PureTLVMessage to the given buffer.
func EncodePureTLVMessage(msg PureTLVMessage, buf *bytes.Buffer) error {
	return EncodeRecordsTo(buf, msg.AllRecords())
}

// UnsignedRangeFunc returns true when a TLV type is in the unsigned range of a
// pure-TLV message (i.e., excluded from the signature). Each protocol supplies
// its own predicate to encode the boundary between signed and unsigned types.
type UnsignedRangeFunc func(tlv.Type) bool

// SerialiseFieldsToSign serialises all the records from the given
// PureTLVMessage that fall within the BOLT 7 v2 signed TLV range. Use
// SerialiseFieldsToSignFn for a protocol with a different boundary.
func SerialiseFieldsToSign(msg PureTLVMessage) ([]byte, error) {
	return SerialiseFieldsToSignFn(msg, InUnsignedRange)
}

// SerialiseFieldsToSignFn serialises all the records from the given
// PureTLVMessage that the supplied predicate keeps in the signed range. A type
// for which isUnsigned returns true is excluded from the digest.
func SerialiseFieldsToSignFn(msg PureTLVMessage,
	isUnsigned UnsignedRangeFunc) ([]byte, error) {

	var signedRecords []tlv.Record
	for _, record := range msg.AllRecords() {
		if isUnsigned(record.Type()) {
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

// InUnsignedRange is the BOLT 7 v2 UnsignedRangeFunc: it returns true for types
// in 160-999_999_999 or 3_000_000_000+, which sit outside the BOLT 7 v2 signed
// ranges (0-159 and 1_000_000_000-2_999_999_999).
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

// ExtraSignedFieldsFromTypeMap returns the unhandled signed-range entries from
// a tlv.TypeMap (as returned by DecodeWithParsedTypes(P2P)) so the caller can
// re-emit them and keep the message signature valid. It uses the BOLT 7 v2
// signed range; use ExtraSignedFieldsFromTypeMapFn for a different boundary.
func ExtraSignedFieldsFromTypeMap(m tlv.TypeMap) ExtraSignedFields {
	return ExtraSignedFieldsFromTypeMapFn(m, InUnsignedRange)
}

// ExtraSignedFieldsFromTypeMapFn returns the unhandled entries from a
// tlv.TypeMap that the supplied predicate keeps in the signed range, so the
// caller can re-emit them and keep the message signature valid. Entries for
// which isUnsigned returns true are dropped.
func ExtraSignedFieldsFromTypeMapFn(m tlv.TypeMap,
	isUnsigned UnsignedRangeFunc) ExtraSignedFields {

	extraFields := make(ExtraSignedFields)
	for t, v := range m {
		// A nil value signals that this type was consumed by one of the
		// typed records passed to the TLV stream decoder, so its bytes
		// are already represented elsewhere and do not need to be
		// tracked here.
		if v == nil {
			continue
		}

		// Types the predicate places outside the signed range fall
		// outside the signature's coverage, so they do not need to
		// survive into re-encoding.
		if isUnsigned(t) {
			continue
		}

		// The remaining types are unhandled but within the signed
		// range; preserve their raw bytes so the message can re-emit
		// them verbatim and the signature stays valid.
		extraFields[uint64(t)] = v
	}

	return extraFields
}
