package lnwire

import (
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// LeaseExpiryType is the type of the experimental record used to
	// communicate the expiration of a channel lease throughout the channel
	// funding process.
	//
	// TODO: Decide on actual TLV type. Custom records start at 2^16.
	LeaseExpiryRecordType tlv.Type = 1 << 16
)

// LeaseExpiry represents the absolute expiration height of a channel lease. All
// outputs that pay directly to the channel initiator are locked until this
// height is reached.
type LeaseExpiry uint32

// Record returns a TLV record that can be used to encode/decode the LeaseExpiry
// type from a given TLV stream.
func (l *LeaseExpiry) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		LeaseExpiryRecordType, l, 4, leaseExpiryEncoder, leaseExpiryDecoder,
	)
}

// leaseExpiryEncoder is a custom TLV encoder for the LeaseExpiry record.
func leaseExpiryEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*LeaseExpiry); ok {
		return tlv.EUint32T(w, uint32(*v), buf)
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.LeaseExpiry")
}

// leaseExpiryDecoder is a custom TLV decoder for the LeaseExpiry record.
func leaseExpiryDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*LeaseExpiry); ok {
		var leaseExpiry uint32
		if err := tlv.DUint32(r, &leaseExpiry, buf, l); err != nil {
			return err
		}
		*v = LeaseExpiry(leaseExpiry)
		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.LeaseExpiry")
}
