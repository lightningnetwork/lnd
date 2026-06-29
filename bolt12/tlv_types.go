package bolt12

import (
	"github.com/lightningnetwork/lnd/tlv"
)

// TUint64 is a uint64 that serializes using truncated encoding (tu64)
// as required by BOLT 12. Leading zero bytes are omitted.
type TUint64 uint64

// Record returns a TLV record using truncated uint64 encoding.
//
// NOTE: This implements the tlv.RecordProducer interface.
func (t *TUint64) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, (*uint64)(t),
		func() uint64 {
			return tlv.SizeTUint64(uint64(*t))
		},
		tlv.ETUint64, tlv.DTUint64,
	)
}

// TUint32 is a uint32 that serializes using truncated encoding (tu32) as
// required by BOLT 12. Leading zero bytes are omitted.
type TUint32 uint32

// Record returns a TLV record using truncated uint32 encoding.
//
// NOTE: This implements the tlv.RecordProducer interface.
func (t *TUint32) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, (*uint32)(t),
		func() uint64 {
			return tlv.SizeTUint32(uint32(*t))
		},
		tlv.ETUint32, tlv.DTUint32,
	)
}
