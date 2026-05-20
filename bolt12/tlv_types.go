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
