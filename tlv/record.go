package tlv

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Type is an 8-bit identifier for a TLV Record.
type Type uint8

// SentinelType is the identifier for the sentinel record. It uses the highest
// possible identifier so that it always occurs at the end of a stream when
// present.
const SentinelType Type = 255

// SizeFunc is a function that can compute the length of a given field. Since
// the size of the underlying field can change, this allows the size of the
// field to be evaluated at the time of encoding.
type SizeFunc func() uint16

// SizeVarBytes is a SizeFunc that can compute the length of a byte slice.
func SizeVarBytes(e *[]byte) SizeFunc {
	return func() uint16 {
		return uint16(len(*e))
	}
}

// Record holds the required information to encode or decode a TLV record.
type Record struct {
	typ        Type
	staticSize uint16
	sizeFunc   SizeFunc
	value      interface{}
}

// Size returns the size of the Record's value. If no static size is known, the
// dynamic size will be evaluated.
func (f *Record) Size() uint16 {
	if f.sizeFunc == nil {
		return f.staticSize
	}

	return f.sizeFunc()
}

// MakeSentinelRecord creates a sentinel record with type 255.
func MakeSentinelRecord() Record {
	return Record{
		typ: SentinelType,
	}
}

// MakePrimitiveRecord creates a record for common types.
func MakePrimitiveRecord(typ Type, val interface{}) Record {
	var (
		staticSize uint16
		sizeFunc   SizeFunc
	)
	switch e := val.(type) {
	case *uint8, *int8:
		staticSize = 1
	case *uint16, *int16:
		staticSize = 2
	case *uint32, *int32:
		staticSize = 4
	case *uint64, *int64:
		staticSize = 8
	case *[32]byte:
		staticSize = 32
	case *[33]byte, **btcec.PublicKey:
		staticSize = 33
	case *lnwire.Sig:
		staticSize = 64
	case *[]byte:
		sizeFunc = SizeVarBytes(e)
	default:
		panic("unknown static type")
	}

	return makeFieldRecord(typ, staticSize, sizeFunc, val)
}

// MakeStaticRecord creates a record for a field of fixed-size
func MakeStaticRecord(typ Type, val interface{}, size uint16) Record {
	return makeFieldRecord(typ, size, nil, val)
}

// MakeDynamicRecord creates a record whose size may vary, and will be
// determined at the time of encoding via sizeFunc.
func MakeDynamicRecord(typ Type, val interface{}, sizeFunc SizeFunc) Record {
	return makeFieldRecord(typ, 0, sizeFunc, val)
}

// makeRetainedRecord creates a record holding an static byte slice. Since no
// encoding is known for this record, the bytes will be reserialized blindly
// during encoding.
func makeRetainedRecord(typ Type, val *[]byte) Record {
	return makeFieldRecord(typ, uint16(len(*val)), nil, val)
}

// makeFieldRecord creates a Record from the given parameters. This method
// panics when trying to construct a sentinel record, which should be done only
// with MakeSentinelRecord.
func makeFieldRecord(typ Type, size uint16, sizeFunc SizeFunc,
	val interface{}) Record {

	// Prevent creation of records that attempt to use the SentinelType to
	// hold information for a field.
	if typ == SentinelType {
		panic("misuse of sentinel type")
	}

	return Record{
		typ:        typ,
		staticSize: size,
		sizeFunc:   sizeFunc,
		value:      val,
	}
}
