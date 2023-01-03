package lnwire

import "github.com/lightningnetwork/lnd/tlv"

const (
	// UpfrontFeeMsatRecordType is the type for upfront fees that pay
	// receiving nodes for the addition of a HTLC.
	//
	// Note: we use type 2 here because type 0 is reserved by the route
	// blinding proposal.
	UpfrontFeeMsatRecordType tlv.Type = 2
)

// UpfrontFee is used to communicate upfront fees. This struct wraps a uint64
// rather than aliasing it so that we can use the built-in TLV encoding/
// decoding functions (aliasing interferes with go's type inference).
type UpfrontFee struct {
	uint64
}

// Value returns the value contained in an upfront fee field, and a boolean
// indicating whether the upfront fee is set (which allows us distinguish
// between populated, zero-value fees and nil values).
func (u *UpfrontFee) Value() (MilliSatoshi, bool) {
	if u == nil {
		return 0, false
	}

	return MilliSatoshi(u.uint64), true
}

// Record returns a TLV record for upfront fees.
//
// Note: implements the RecordProducer interface.
func (u *UpfrontFee) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		UpfrontFeeMsatRecordType, &u.uint64, func() uint64 {
			return tlv.SizeTUint64(u.uint64)
		}, tlv.ETUint64, tlv.DTUint64,
	)
}
