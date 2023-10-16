package tlv

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// harness decodes the passed data, re-encodes it, and verifies that the
// re-encoded data matches the original data.
func harness(t *testing.T, data []byte, encode Encoder, decode Decoder,
	val interface{}, decodeLen uint64) {

	if uint64(len(data)) > decodeLen {
		return
	}

	r := bytes.NewReader(data)

	var buf [8]byte
	if err := decode(r, val, &buf, decodeLen); err != nil {
		return
	}

	var b bytes.Buffer
	require.NoError(t, encode(&b, val, &buf))

	// Use bytes.Equal instead of require.Equal so that nil and empty slices
	// are considered equal.
	require.True(
		t, bytes.Equal(data, b.Bytes()), "%v != %v", data, b.Bytes(),
	)
}

func FuzzUint8(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint8
		harness(t, data, EUint8, DUint8, &val, 1)
	})
}

func FuzzUint16(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint16
		harness(t, data, EUint16, DUint16, &val, 2)
	})
}

func FuzzUint32(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint32
		harness(t, data, EUint32, DUint32, &val, 4)
	})
}

func FuzzUint64(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint64
		harness(t, data, EUint64, DUint64, &val, 8)
	})
}

func FuzzBytes32(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val [32]byte
		harness(t, data, EBytes32, DBytes32, &val, 32)
	})
}

func FuzzBytes33(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val [33]byte
		harness(t, data, EBytes33, DBytes33, &val, 33)
	})
}

func FuzzBytes64(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val [64]byte
		harness(t, data, EBytes64, DBytes64, &val, 64)
	})
}

func FuzzPubKey(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val *btcec.PublicKey
		harness(t, data, EPubKey, DPubKey, &val, 33)
	})
}

func FuzzVarBytes(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val []byte
		harness(t, data, EVarBytes, DVarBytes, &val, uint64(len(data)))
	})
}

func FuzzBool(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val bool
		harness(t, data, EBool, DBool, &val, 1)
	})
}

// bigSizeHarness works the same as harness, except that it compares decoded
// values instead of encoded values. We do this because DBigSize may leave some
// bytes unparsed from data, causing the encoded data to be shorter than the
// original.
func bigSizeHarness(t *testing.T, data []byte, val1, val2 interface{}) {
	if len(data) > 9 {
		return
	}

	r := bytes.NewReader(data)

	var buf [8]byte
	if err := DBigSize(r, val1, &buf, 9); err != nil {
		return
	}

	var b bytes.Buffer
	require.NoError(t, EBigSize(&b, val1, &buf))

	r2 := bytes.NewReader(b.Bytes())
	require.NoError(t, DBigSize(r2, val2, &buf, 9))

	require.Equal(t, val1, val2)
}

func FuzzBigSize32(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val1, val2 uint32
		bigSizeHarness(t, data, &val1, &val2)
	})
}

func FuzzBigSize64(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val1, val2 uint64
		bigSizeHarness(t, data, &val1, &val2)
	})
}

func FuzzTUint16(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint16
		for decodeLen := 0; decodeLen <= 2; decodeLen++ {
			harness(
				t, data, ETUint16, DTUint16, &val,
				uint64(decodeLen),
			)
		}
	})
}

func FuzzTUint32(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint32
		for decodeLen := 0; decodeLen <= 4; decodeLen++ {
			harness(
				t, data, ETUint32, DTUint32, &val,
				uint64(decodeLen),
			)
		}
	})
}

func FuzzTUint64(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint64
		for decodeLen := 0; decodeLen <= 8; decodeLen++ {
			harness(
				t, data, ETUint64, DTUint64, &val,
				uint64(decodeLen),
			)
		}
	})
}

// encodeParsedTypes re-encodes TLVs decoded from a stream, using the
// parsedTypes and decodedRecords produced during decoding. This function
// requires that each record in decodedRecords has a type number equivalent to
// its index in the slice.
func encodeParsedTypes(t *testing.T, parsedTypes TypeMap,
	decodedRecords []Record) []byte {

	var encodeRecords []Record
	for typ, val := range parsedTypes {
		// If typ is present in decodedRecords, use the decoded value.
		if typ < Type(len(decodedRecords)) {
			encodeRecords = append(
				encodeRecords, decodedRecords[typ],
			)
			continue
		}

		// Otherwise, typ is not present in decodedRecords, and we must
		// create a new one.
		val := val
		encodeRecords = append(
			encodeRecords, MakePrimitiveRecord(typ, &val),
		)
	}
	SortRecords(encodeRecords)
	encodeStream := MustNewStream(encodeRecords...)

	var b bytes.Buffer
	require.NoError(t, encodeStream.Encode(&b))

	return b.Bytes()
}

// FuzzStream does two stream decode-encode cycles on the fuzzer data and checks
// that the encoded values match.
func FuzzStream(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var (
			u8      uint8
			u16     uint16
			u32     uint32
			u64     uint64
			b32     [32]byte
			b33     [33]byte
			b64     [64]byte
			pk      *btcec.PublicKey
			b       []byte
			bs32    uint32
			bs64    uint64
			tu16    uint16
			tu32    uint32
			tu64    uint64
			boolean bool
		)

		sizeTU16 := func() uint64 {
			return SizeTUint16(tu16)
		}
		sizeTU32 := func() uint64 {
			return SizeTUint32(tu32)
		}
		sizeTU64 := func() uint64 {
			return SizeTUint64(tu64)
		}

		// We deliberately set each record's type number to its index in
		// the slice, as this simplifies the re-encoding logic in
		// encodeParsedTypes().
		decodeRecords := []Record{
			MakePrimitiveRecord(0, &u8),
			MakePrimitiveRecord(1, &u16),
			MakePrimitiveRecord(2, &u32),
			MakePrimitiveRecord(3, &u64),
			MakePrimitiveRecord(4, &b32),
			MakePrimitiveRecord(5, &b33),
			MakePrimitiveRecord(6, &b64),
			MakePrimitiveRecord(7, &pk),
			MakePrimitiveRecord(8, &b),
			MakeBigSizeRecord(9, &bs32),
			MakeBigSizeRecord(10, &bs64),
			MakeDynamicRecord(
				11, &tu16, sizeTU16, ETUint16, DTUint16,
			),
			MakeDynamicRecord(
				12, &tu32, sizeTU32, ETUint32, DTUint32,
			),
			MakeDynamicRecord(
				13, &tu64, sizeTU64, ETUint64, DTUint64,
			),
			MakePrimitiveRecord(14, &boolean),
		}
		decodeStream := MustNewStream(decodeRecords...)

		r := bytes.NewReader(data)

		// Use the P2P decoding method to avoid OOMs from large lengths
		// in the fuzzer TLV data.
		parsedTypes, err := decodeStream.DecodeWithParsedTypesP2P(r)
		if err != nil {
			return
		}

		encoded := encodeParsedTypes(t, parsedTypes, decodeRecords)

		r2 := bytes.NewReader(encoded)
		decodeStream2 := MustNewStream(decodeRecords...)

		// The P2P decoding method is not required here since we're now
		// decoding TLV data that we created (not the fuzzer).
		parsedTypes2, err := decodeStream2.DecodeWithParsedTypes(r2)
		require.NoError(t, err)

		encoded2 := encodeParsedTypes(t, parsedTypes2, decodeRecords)

		require.Equal(t, encoded, encoded2)
	})
}
