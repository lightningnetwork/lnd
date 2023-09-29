package lnwire

import (
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// BooleanRecordProducer wraps a boolean with the tlv type it should be encoded
// with. A boolean is by default false, if the TLV is present then the boolean
// is true.
type BooleanRecordProducer struct {
	Bool bool
	Type tlv.Type
}

// Record returns the tlv record for the boolean entry.
func (b *BooleanRecordProducer) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		b.Type, &b.Bool, 0, booleanEncoder, booleanDecoder,
	)
}

// NewBooleanRecordProducer constructs a new BooleanRecordProducer.
func NewBooleanRecordProducer(t tlv.Type) *BooleanRecordProducer {
	return &BooleanRecordProducer{
		Type: t,
	}
}

func booleanEncoder(_ io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*bool); ok {
		if !*v {
			return fmt.Errorf("a boolean record should only be " +
				"encoded if the value of the boolean is true")
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "bool")
}

func booleanDecoder(_ io.Reader, val interface{}, _ *[8]byte, _ uint64) error {
	if v, ok := val.(*bool); ok {
		*v = true

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "bool")
}
