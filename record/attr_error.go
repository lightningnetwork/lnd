package record

import (
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// AttributableErrorOnionType is the type used in the onion for the fat
	// error message structure.
	AttributableErrorOnionType tlv.Type = 20

	// attributableErrorLength is the byte size of the attributable error
	// record.
	attributableErrorLength = 0
)

// AttributableError is a record that encodes the fields necessary for fat
// errors.
type AttributableError struct{}

// NewAttributableError generates a new AttributableError record with the given
// structure.
func NewAttributableError() *AttributableError {
	return &AttributableError{}
}

// AttributableErrorEncoder writes the AttributableError record to the provided
// io.Writer.
func AttributableErrorEncoder(_ io.Writer, val interface{},
	_ *[8]byte) error {

	_, ok := val.(*AttributableError)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "AttributableError")
	}

	return nil
}

// AttributableErrorDecoder reads the AttributableError record to the provided
// io.Reader.
func AttributableErrorDecoder(_ io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	_, ok := val.(*AttributableError)
	if !ok || l != attributableErrorLength {
		return tlv.NewTypeForDecodingErr(
			val, "AttributableError", l, attributableErrorLength,
		)
	}

	return nil
}

// Record returns a tlv.Record that can be used to encode or decode this record.
func (r *AttributableError) Record() tlv.Record {
	size := func() uint64 {
		return attributableErrorLength
	}

	return tlv.MakeDynamicRecord(
		AttributableErrorOnionType, r, size, AttributableErrorEncoder,
		AttributableErrorDecoder,
	)
}

// PayloadSize returns the size this record takes up in encoded form.
func (r *AttributableError) PayloadSize() uint64 {
	return attributableErrorLength
}

// String returns a human-readable representation of the AttributableError
// payload field.
func (r *AttributableError) String() string {
	if r == nil {
		return "<nil>"
	}

	return "attr errors"
}
