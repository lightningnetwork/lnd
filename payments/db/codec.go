package paymentsdb

import (
	"encoding/binary"
	"io"

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
		if _, ok := err.(UnknownElementType); !ok {
			return err
		}
	}

	// Process any wtdb-specific extensions to the codec.
	switch e := element.(type) {

	case *paymentIndexType:
		if err := binary.Read(r, byteOrder, e); err != nil {
			return err
		}

	// Type is still unknown to wtdb extensions, fail.
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
		if _, ok := err.(UnknownElementType); !ok {
			return err
		}
	}

	// Process any wtdb-specific extensions to the codec.
	switch e := element.(type) {

	case paymentIndexType:
		if err := binary.Write(w, byteOrder, e); err != nil {
			return err
		}

	// Type is still unknown to wtdb extensions, fail.
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
