package record

import (
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// AMPOnionType is the type used in the onion to reference the AMP fields:
// root_share, set_id, and child_index.
const AMPOnionType tlv.Type = 14

// AMP is a record that encodes the fields necessary for atomic multi-path
// payments.
type AMP struct {
	rootShare  [32]byte
	setID      [32]byte
	childIndex uint32
}

// MaxAmpPayLoadSize is an AMP Record which when serialized to a tlv record uses
// the maximum payload size. The `childIndex` is created randomly and is a
// 4 byte `varint` type so we make sure we use an index which will be encoded in
// 4 bytes.
var MaxAmpPayLoadSize = AMP{
	rootShare:  [32]byte{},
	setID:      [32]byte{},
	childIndex: 0x80000000,
}

// NewAMP generate a new AMP record with the given root_share, set_id, and
// child_index.
func NewAMP(rootShare, setID [32]byte, childIndex uint32) *AMP {
	return &AMP{
		rootShare:  rootShare,
		setID:      setID,
		childIndex: childIndex,
	}
}

// RootShare returns the root share contained in the AMP record.
func (a *AMP) RootShare() [32]byte {
	return a.rootShare
}

// SetID returns the set id contained in the AMP record.
func (a *AMP) SetID() [32]byte {
	return a.setID
}

// ChildIndex returns the child index contained in the AMP record.
func (a *AMP) ChildIndex() uint32 {
	return a.childIndex
}

// AMPEncoder writes the AMP record to the provided io.Writer.
func AMPEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*AMP); ok {
		if err := tlv.EBytes32(w, &v.rootShare, buf); err != nil {
			return err
		}

		if err := tlv.EBytes32(w, &v.setID, buf); err != nil {
			return err
		}

		return tlv.ETUint32T(w, v.childIndex, buf)
	}
	return tlv.NewTypeForEncodingErr(val, "AMP")
}

const (
	// minAMPLength is the minimum length of a serialized AMP TLV record,
	// which occurs when the truncated encoding of child_index takes 0
	// bytes, leaving only the root_share and set_id.
	minAMPLength = 64

	// maxAMPLength is the maximum length of a serialized AMP TLV record,
	// which occurs when the truncated encoding of a child_index takes 2
	// bytes.
	maxAMPLength = 68
)

// AMPDecoder reads the AMP record from the provided io.Reader.
func AMPDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*AMP); ok && minAMPLength <= l && l <= maxAMPLength {
		if err := tlv.DBytes32(r, &v.rootShare, buf, 32); err != nil {
			return err
		}

		if err := tlv.DBytes32(r, &v.setID, buf, 32); err != nil {
			return err
		}

		return tlv.DTUint32(r, &v.childIndex, buf, l-minAMPLength)
	}
	return tlv.NewTypeForDecodingErr(val, "AMP", l, maxAMPLength)
}

// Record returns a tlv.Record that can be used to encode or decode this record.
func (a *AMP) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		AMPOnionType, a, a.PayloadSize, AMPEncoder, AMPDecoder,
	)
}

// PayloadSize returns the size this record takes up in encoded form.
func (a *AMP) PayloadSize() uint64 {
	return 32 + 32 + tlv.SizeTUint32(a.childIndex)
}

// String returns a human-readable description of the amp payload fields.
func (a *AMP) String() string {
	if a == nil {
		return "<nil>"
	}

	return fmt.Sprintf("root_share=%x set_id=%x child_index=%d",
		a.rootShare, a.setID, a.childIndex)
}
