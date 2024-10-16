package lnwire

import "github.com/lightningnetwork/lnd/tlv"

// QueryEncoding is an enum-like type that represents exactly how a set data is
// encoded on the wire.
type QueryEncoding uint8

const (
	// EncodingSortedPlain signals that the set of data is encoded using the
	// regular encoding, in a sorted order.
	EncodingSortedPlain QueryEncoding = 0

	// EncodingSortedZlib signals that the set of data is encoded by first
	// sorting the set of channel ID's, as then compressing them using zlib.
	//
	// NOTE: this should no longer be used or accepted.
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
