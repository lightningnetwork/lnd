package channeldb

import (
	"io"
	"time"

	cstate "github.com/lightningnetwork/lnd/chanstate"
)

// UnknownElementType is an error returned when the codec is unable to encode or
// decode a particular type.
type UnknownElementType = cstate.UnknownElementType

// NewUnknownElementType creates a new UnknownElementType error from the passed
// method name and element.
func NewUnknownElementType(method string, el interface{}) UnknownElementType {
	return cstate.NewUnknownElementType(method, el)
}

// WriteElement is a one-stop shop to write the big endian representation of
// any element which is to be serialized for storage on disk. The passed
// io.Writer should be backed by an appropriately sized byte slice, or be able
// to dynamically expand to accommodate additional data.
func WriteElement(w io.Writer, element interface{}) error {
	return cstate.WriteElement(w, element)
}

// WriteElements is writes each element in the elements slice to the passed
// io.Writer using WriteElement.
func WriteElements(w io.Writer, elements ...interface{}) error {
	return cstate.WriteElements(w, elements...)
}

// ReadElement is a one-stop utility function to deserialize any datastructure
// encoded using the serialization format of the database.
func ReadElement(r io.Reader, element interface{}) error {
	return cstate.ReadElement(r, element)
}

// ReadElements deserializes a variable number of elements into the passed
// io.Reader, with each element being deserialized according to the ReadElement
// function.
func ReadElements(r io.Reader, elements ...interface{}) error {
	return cstate.ReadElements(r, elements...)
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
