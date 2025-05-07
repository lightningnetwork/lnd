package tlv

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
)

// Type is an 64-bit identifier for a TLV Record.
type Type uint64

// TypeMap is a map of parsed Types. The map values are byte slices. If the byte
// slice is nil, the type was successfully parsed. Otherwise the value is byte
// slice containing the encoded data.
type TypeMap map[Type][]byte

// Blob is a type alias for a byte slice. It's used to indicate that a slice of
// bytes is actually an encoded TLV stream.
type Blob = []byte

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

// RecorderProducer is an interface for objects that can produce a Record object
// capable of encoding and/or decoding the RecordProducer as a Record.
type RecordProducer interface {
	// Record returns a Record that can be used to encode or decode the
	// backing object.
	Record() Record
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

// Record (the function) is the trivial implementation of RecordProducer for
// Record (the type). This makes it seamless to mix primitive and dynamic
// records together in the same collections.
//
// NOTE: Part of the RecordProducer interface.
func (f *Record) Record() Record {
	return *f
}

// Size returns the size of the Record's value. If no static size is known, the
// dynamic size will be evaluated.
func (f *Record) Size() uint64 {
	if f.sizeFunc == nil {
		return f.staticSize
	}

	return f.sizeFunc()
}

// Type returns the type of the underlying TLV record.
func (f *Record) Type() Type {
	return f.typ
}

// Encode writes out the TLV record to the passed writer. This is useful when a
// caller wants to obtain the raw encoding of a *single* TLV record, outside
// the context of the Stream struct.
func (f *Record) Encode(w io.Writer) error {
	var b [8]byte

	return f.encoder(w, f.value, &b)
}

// Decode read in the TLV record from the passed reader. This is useful when a
// caller wants decode a *single* TLV record, outside the context of the Stream
// struct.
func (f *Record) Decode(r io.Reader, l uint64) error {
	var b [8]byte
	return f.decoder(r, f.value, &b, l)
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
	case *bool:
		staticSize = 1
		encoder = EBool
		decoder = DBool

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
		panic(fmt.Sprintf("unknown primitive type: %T", val))
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

// RecordsToMap encodes a series of TLV records as raw key-value pairs in the
// form of a map.
func RecordsToMap(records []Record) (map[uint64][]byte, error) {
	tlvMap := make(map[uint64][]byte, len(records))

	for _, record := range records {
		var b bytes.Buffer
		if err := record.Encode(&b); err != nil {
			return nil, err
		}

		tlvMap[uint64(record.Type())] = b.Bytes()
	}

	return tlvMap, nil
}

// StubEncoder is a factory function that makes a stub tlv.Encoder out of a raw
// value. We can use this to make a record that can be encoded when we don't
// actually know it's true underlying value, and only it serialization.
func StubEncoder(v []byte) Encoder {
	return func(w io.Writer, val interface{}, buf *[8]byte) error {
		_, err := w.Write(v)
		return err
	}
}

// MapToRecords encodes the passed TLV map as a series of regular tlv.Record
// instances. The resulting set of records will be returned in sorted order by
// their type.
func MapToRecords(tlvMap map[uint64][]byte) []Record {
	records := make([]Record, 0, len(tlvMap))
	for k, v := range tlvMap {
		// We don't pass in a decoder here since we don't actually know
		// the type, and only expect this Record to be used for display
		// and encoding purposes.
		record := MakeStaticRecord(
			Type(k), nil, uint64(len(v)), StubEncoder(v), nil,
		)

		records = append(records, record)
	}

	SortRecords(records)

	return records
}

// SortRecords is a helper function that will sort a slice of records in place
// according to their type.
func SortRecords(records []Record) {
	if len(records) == 0 {
		return
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Type() < records[j].Type()
	})
}

// MakeBigSizeRecord creates a tlv record using the BigSize format. The only
// allowed values are uint64 and uint32.
//
// NOTE: for uint32, we would only gain space reduction if the encoded value is
// no greater than 65535, which requires at most 3 bytes to encode.
func MakeBigSizeRecord[T constraintUint32Or64](typ Type, val *T) Record {
	var (
		staticSize uint64
		sizeFunc   SizeFunc
		encoder    Encoder
		decoder    Decoder
	)
	switch any(val).(type) {
	case *uint32:
		sizeFunc = SizeBigSize(val)
		encoder = EBigSize
		decoder = DBigSize

	case *uint64:
		sizeFunc = SizeBigSize(val)
		encoder = EBigSize
		decoder = DBigSize

	default:
		panic(fmt.Sprintf("unknown supported compact type: %T", val))
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
