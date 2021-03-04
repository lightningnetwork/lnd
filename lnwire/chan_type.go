package lnwire

import (
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// ChanTypeRecordType is the type of the experimental record used to
	// denote which channel type is being negotiated.
	//
	// TODO(roasbeef): move these into the TLV package?
	ChanTypeRecordType = 2021
)

// ChannelType is an enum-like value that denotes the explicit channel type
// that is currently being negotiated.
type ChannelType uint16

const (
	// ChanTypeBase is the very first channel type added to the Lightning
	// Network.
	ChanTypeBase = 0

	// ChanTypeTweakless denotes a modification on the base channel type
	// that removes a randomized nonce used to compute the pkScript of the
	// remote party during a force close.
	ChanTypeTweakless = 1

	// ChanTypeZeroFeeAnchors denotes a modification on the ChanTypeAnchors
	// type, that moved to ensure there're no explicit fees paid at the
	// second level, and they must instead be paid by adding a new input.
	ChanTypeZeroFeeAnchors = 2
)

// String returns a human readable representation of the target ChannelType
// field.
func (c ChannelType) String() string {
	switch c {
	case ChanTypeBase:
		return "ChanTypeBase"

	case ChanTypeTweakless:
		return "ChanTypeTweakless"

	case ChanTypeZeroFeeAnchors:
		return "ChanTypeZeroFeeAnchors"

	default:
		return fmt.Sprintf("<UnknownChannelType(%v)>", uint16(c))
	}

}

// eChanType is a custom TLV encoder for the ChannelType record.
func eChanType(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*ChannelType); ok {
		return tlv.EUint16T(w, uint16(*v), buf)
	}
	return tlv.NewTypeForEncodingErr(val, "lnwire.ChannelType")
}

// dChanType is a custom TLV decode for the ChannelType record.
func dChanType(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*ChannelType); ok {
		var chanType uint16
		err := tlv.DUint16(r, &chanType, buf, l)
		if err != nil {
			return err
		}
		*v = ChannelType(chanType)
		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.ChannelType")
}

// Record returns a TLV record that can be used to encode/decode the channel
// type from a given TLV stream.
func (c *ChannelType) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		ChanTypeRecordType, c,
		2,
		eChanType, dChanType,
	)
}
