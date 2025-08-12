package paymentsdb

import (
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
)

// Big endian is the preferred byte order, due to cursor scans over
// integer keys iterating in order.
var byteOrder = binary.BigEndian

// UnknownElementType is an alias for channeldb.UnknownElementType.
type UnknownElementType = channeldb.UnknownElementType

// ReadElement deserializes a single element from the provided io.Reader.
func ReadElement(r io.Reader, element interface{}) error {
	err := channeldb.ReadElement(r, element)
	switch {
	// Known to channeldb codec.
	case err == nil:
		return nil

	// Fail if error is not UnknownElementType.
	default:
		var unknownElementType UnknownElementType
		if !errors.As(err, &unknownElementType) {
			return err
		}
	}

	// Process any paymentsdb-specific extensions to the codec.
	switch e := element.(type) {
	case *paymentIndexType:
		if err := binary.Read(r, byteOrder, e); err != nil {
			return err
		}

	// Type is still unknown to paymentsdb extensions, fail.
	default:
		return channeldb.NewUnknownElementType(
			"ReadElement", element,
		)
	}

	return nil
}

// WriteElement serializes a single element into the provided io.Writer.
func WriteElement(w io.Writer, element interface{}) error {
	err := channeldb.WriteElement(w, element)
	switch {
	// Known to channeldb codec.
	case err == nil:
		return nil

	// Fail if error is not UnknownElementType.
	default:
		var unknownElementType UnknownElementType
		if !errors.As(err, &unknownElementType) {
			return err
		}
	}

	// Process any paymentsdb-specific extensions to the codec.
	switch e := element.(type) {
	case paymentIndexType:
		if err := binary.Write(w, byteOrder, e); err != nil {
			return err
		}

	// Type is still unknown to paymentsdb extensions, fail.
	default:
		return channeldb.NewUnknownElementType(
			"WriteElement", element,
		)
	}

	return nil
}

// WriteElements serializes a variadic list of elements into the given
// io.Writer.
func WriteElements(w io.Writer, elements ...interface{}) error {
	for _, element := range elements {
		if err := WriteElement(w, element); err != nil {
			return err
		}
	}

	return nil
}

// ReadElements deserializes the provided io.Reader into a variadic list of
// target elements.
func ReadElements(r io.Reader, elements ...interface{}) error {
	for _, element := range elements {
		if err := ReadElement(r, element); err != nil {
			return err
		}
	}

	return nil
}

// deserializeTime deserializes time as unix nanoseconds.
func deserializeTime(r io.Reader) (time.Time, error) {
	var scratch [8]byte
	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return time.Time{}, err
	}

	// Convert to time.Time. Interpret unix nano time zero as a zero
	// time.Time value.
	unixNano := byteOrder.Uint64(scratch[:])
	if unixNano == 0 {
		return time.Time{}, nil
	}

	return time.Unix(0, int64(unixNano)), nil
}

// serializeTime serializes time as unix nanoseconds.
func serializeTime(w io.Writer, t time.Time) error {
	var scratch [8]byte

	// Convert to unix nano seconds, but only if time is non-zero. Calling
	// UnixNano() on a zero time yields an undefined result.
	var unixNano int64
	if !t.IsZero() {
		unixNano = t.UnixNano()
	}

	byteOrder.PutUint64(scratch[:], uint64(unixNano))
	_, err := w.Write(scratch[:])

	return err
}
