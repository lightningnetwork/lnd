package lnwire

import (
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// TrueBoolean is a record that indicates true or false using the presence of
// the record. If the record is absent, it indicates false. If it is present,
// it indicates true.
type TrueBoolean struct{}

// Record returns the tlv record for the boolean entry.
func (b *TrueBoolean) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, b, 0, booleanEncoder, booleanDecoder,
	)
}

func booleanEncoder(_ io.Writer, val interface{}, _ *[8]byte) error {
	if _, ok := val.(*TrueBoolean); ok {
		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "TrueBoolean")
}

func booleanDecoder(_ io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if _, ok := val.(*TrueBoolean); ok && (l == 0 || l == 1) {
		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "TrueBoolean")
}
