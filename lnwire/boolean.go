package lnwire

import (
	"errors"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// Boolean wraps a boolean in a struct to help it satisfy the tlv.RecordProducer
// interface. If a boolean tlv record is not present, this has a meaning of
// false. If it is present but has a length of 0, then this means true.
// Otherwise, if it is present but has a length of 1 then the value has been
// encoded explicitly.
type Boolean struct {
	B bool
}

// Record returns the tlv record for the boolean entry.
func (b *Boolean) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, &b.B, b.size, booleanEncoder, booleanDecoder,
	)
}

// size returns the number of bytes required to encode the Boolean. If the
// underlying bool is true, then we will have a zero length tlv record,
// otherwise we will have a 1 byte record.
func (b *Boolean) size() uint64 {
	if b.B {
		return 0
	}

	return 1
}

func booleanEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*bool); ok {
		// If the underlying value is true, then we can just make the
		// tlv zero value as that implies true.
		if *v {
			return nil
		}

		// If it is false, then we encode it explicitly.
		buf[0] = 0
		_, err := w.Write(buf[:1])

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "bool")
}

func booleanDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*bool); ok && (l == 0 || l == 1) {
		// If the length is zero, then the value is true.
		if l == 0 {
			*v = true

			return nil
		}

		// Else, the length is 1 and the value will have been encoded
		// explicitly.
		if _, err := io.ReadFull(r, buf[:1]); err != nil {
			return err
		}
		if buf[0] != 0 && buf[0] != 1 {
			return errors.New("corrupted data")
		}
		*v = buf[0] != 0

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "bool")
}
