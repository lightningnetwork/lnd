package record

import (
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// AmtOnionType is the type used in the onion to refrence the amount to
	// send to the next hop.
	AmtOnionType tlv.Type = 2

	// LockTimeTLV is the type used in the onion to refenernce the CLTV
	// value that should be used for the next hop's HTLC.
	LockTimeOnionType tlv.Type = 4

	// NextHopOnionType is the type used in the onion to reference the ID
	// of the next hop.
	NextHopOnionType tlv.Type = 6
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
