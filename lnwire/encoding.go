package lnwire

import "github.com/lightningnetwork/lnd/tlv"

// QueryEncoding is an enum-like type that represents exactly how a set data is
// encoded on the wire.
type QueryEncoding uint8

const (
	// EncodingSortedPlain signals that the set of data is encoded using the
	// regular encoding, in a sorted order.
	EncodingSortedPlain QueryEncoding = 0

	// EncodingSortedZlib signals that the set of data was encoded using
	// zlib compression. This encoding was dropped from the BOLT 7 spec
	// and is no longer supported. The constant is retained for detection
	// on the decode side only — it must never be used for encoding.
	//
	// Deprecated: zlib encoding is not accepted or produced by lnd.
	EncodingSortedZlib QueryEncoding = 1
)

// recordProducer is a simple helper struct that implements the
// tlv.RecordProducer interface.
type recordProducer struct {
	record tlv.Record
}

// Record returns the underlying record.
func (r *recordProducer) Record() tlv.Record {
	return r.record
}

// Ensure that recordProducer implements the tlv.RecordProducer interface.
var _ tlv.RecordProducer = (*recordProducer)(nil)
