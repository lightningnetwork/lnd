package migration1

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Big endian is the preferred byte order, due to cursor scans over
// integer keys iterating in order.
var byteOrder = binary.BigEndian

// UnknownElementType is an error returned when the codec is unable to encode
// or decode a particular type.
//
// NOTE: This is a frozen, self-contained copy of the serialization error type
// to ensure that this migration package has no dependency on live packages
// whose implementation may change after this migration was released.
type UnknownElementType struct {
	method  string
	element interface{}
}

// NewUnknownElementType creates a new UnknownElementType error from the passed
// method name and element.
func NewUnknownElementType(method string, el interface{}) UnknownElementType {
	return UnknownElementType{method: method, element: el}
}

// Error returns the name of the method that encountered the error, as well as
// the type that was unsupported.
func (e UnknownElementType) Error() string {
	return fmt.Sprintf("Unknown type in %s: %T", e.method, e.element)
}

// WriteElement serializes a single element into the provided io.Writer using
// big-endian byte order. Only the types required by the migration1 package are
// supported. This is a frozen, self-contained copy of the serialization logic
// to ensure that this migration package has no dependency on live packages
// whose implementation may change after this migration was released.
func WriteElement(w io.Writer, element interface{}) error {
	switch e := element.(type) {
	case paymentIndexType:
		if err := binary.Write(w, byteOrder, e); err != nil {
			return err
		}

	case uint8:
		if err := binary.Write(w, byteOrder, e); err != nil {
			return err
		}

	case uint32:
		if err := binary.Write(w, byteOrder, e); err != nil {
			return err
		}

	case uint64:
		if err := binary.Write(w, byteOrder, e); err != nil {
			return err
		}

	case lnwire.MilliSatoshi:
		if err := binary.Write(w, byteOrder, uint64(e)); err != nil {
			return err
		}

	case [32]byte:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case []byte:
		if err := wire.WriteVarBytes(w, 0, e); err != nil {
			return err
		}

	default:
		return UnknownElementType{"WriteElement", e}
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

// ReadElement deserializes a single element from the provided io.Reader using
// big-endian byte order. Only the types required by the migration1 package are
// supported. This is a frozen, self-contained copy of the deserialization
// logic to ensure that this migration package has no dependency on live
// packages whose implementation may change after this migration was released.
func ReadElement(r io.Reader, element interface{}) error {
	switch e := element.(type) {
	case *paymentIndexType:
		if err := binary.Read(r, byteOrder, e); err != nil {
			return err
		}

	case *uint8:
		if err := binary.Read(r, byteOrder, e); err != nil {
			return err
		}

	case *uint32:
		if err := binary.Read(r, byteOrder, e); err != nil {
			return err
		}

	case *uint64:
		if err := binary.Read(r, byteOrder, e); err != nil {
			return err
		}

	case *lnwire.MilliSatoshi:
		var a uint64
		if err := binary.Read(r, byteOrder, &a); err != nil {
			return err
		}
		*e = lnwire.MilliSatoshi(a)

	case *[32]byte:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case *[]byte:
		b, err := wire.ReadVarBytes(r, 0, 66000, "[]byte")
		if err != nil {
			return err
		}
		*e = b

	default:
		return UnknownElementType{"ReadElement", e}
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
