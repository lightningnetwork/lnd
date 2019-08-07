package tlv

import (
	"io"

	"github.com/btcsuite/btcd/btcec"
)

// Type is an 64-bit identifier for a TLV Record.
type Type uint64

// Encoder is a signature for methods that can encode TLV values. An error
// should be returned if the Encoder cannot support the underlying type of val.
// The provided scratch buffer must be non-nil.
type Encoder func(w io.Writer, val interface{}, buf *[8]byte) error

// Decoder is a signature for methods that can decode TLV values. An error
// should be returned if the Decoder cannot support the underlying type of val.
// The provided scratch buffer must be non-nil.
type Decoder func(r io.Reader, val interface{}, buf *[8]byte, l uint64) error

// ENOP is an encoder that doesn't modify the io.Writer and never fails.
func ENOP(io.Writer, interface{}, *[8]byte) error { return nil }

// DNOP is an encoder that doesn't modify the io.Reader and never fails.
func DNOP(io.Reader, interface{}, *[8]byte, uint64) error { return nil }

// SizeFunc is a function that can compute the length of a given field. Since
// the size of the underlying field can change, this allows the size of the
// field to be evaluated at the time of encoding.
type SizeFunc func() uint64

// SizeVarBytes returns a SizeFunc that can compute the length of a byte slice.
func SizeVarBytes(e *[]byte) SizeFunc {
	return func() uint64 {
		return uint64(len(*e))
	}
}

// Record holds the required information to encode or decode a TLV record.
type Record struct {
	value      interface{}
	typ        Type
	staticSize uint64
	sizeFunc   SizeFunc
	encoder    Encoder
	decoder    Decoder
}

// Size returns the size of the Record's value. If no static size is known, the
// dynamic size will be evaluated.
func (f *Record) Size() uint64 {
	if f.sizeFunc == nil {
		return f.staticSize
	}

	return f.sizeFunc()
}

// MakePrimitiveRecord creates a record for common types.
func MakePrimitiveRecord(typ Type, val interface{}) Record {
	var (
		staticSize uint64
		sizeFunc   SizeFunc
		encoder    Encoder
		decoder    Decoder
	)
	switch e := val.(type) {
	case *uint8:
		staticSize = 1
		encoder = EUint8
		decoder = DUint8

	case *uint16:
		staticSize = 2
		encoder = EUint16
		decoder = DUint16

	case *uint32:
		staticSize = 4
		encoder = EUint32
		decoder = DUint32

	case *uint64:
		staticSize = 8
		encoder = EUint64
		decoder = DUint64

	case *[32]byte:
		staticSize = 32
		encoder = EBytes32
		decoder = DBytes32

	case *[33]byte:
		staticSize = 33
		encoder = EBytes33
		decoder = DBytes33

	case **btcec.PublicKey:
		staticSize = 33
		encoder = EPubKey
		decoder = DPubKey

	case *[64]byte:
		staticSize = 64
		encoder = EBytes64
		decoder = DBytes64

	case *[]byte:
		sizeFunc = SizeVarBytes(e)
		encoder = EVarBytes
		decoder = DVarBytes

	default:
		panic("unknown primitive type")
	}

	return Record{
		value:      val,
		typ:        typ,
		staticSize: staticSize,
		sizeFunc:   sizeFunc,
		encoder:    encoder,
		decoder:    decoder,
	}
}

// MakeStaticRecord creates a record for a field of fixed-size
func MakeStaticRecord(typ Type, val interface{}, size uint64, encoder Encoder,
	decoder Decoder) Record {

	return Record{
		value:      val,
		typ:        typ,
		staticSize: size,
		encoder:    encoder,
		decoder:    decoder,
	}
}

// MakeDynamicRecord creates a record whose size may vary, and will be
// determined at the time of encoding via sizeFunc.
func MakeDynamicRecord(typ Type, val interface{}, sizeFunc SizeFunc,
	encoder Encoder, decoder Decoder) Record {

	return Record{
		value:    val,
		typ:      typ,
		sizeFunc: sizeFunc,
		encoder:  encoder,
		decoder:  decoder,
	}
}
