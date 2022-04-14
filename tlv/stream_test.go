package tlv_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

type parsedTypeTest struct {
	name           string
	encode         []tlv.Type
	decode         []tlv.Type
	expParsedTypes tlv.TypeMap
}

// TestParsedTypes asserts that a Stream will properly return the set of types
// that it encounters when the type is known-and-decoded or unknown-and-ignored.
func TestParsedTypes(t *testing.T) {
	const (
		knownType       = 1
		unknownType     = 3
		secondKnownType = 4
	)

	tests := []parsedTypeTest{
		{
			name:   "known and unknown",
			encode: []tlv.Type{knownType, unknownType},
			decode: []tlv.Type{knownType},
			expParsedTypes: tlv.TypeMap{
				unknownType: []byte{0, 0, 0, 0, 0, 0, 0, 0},
				knownType:   nil,
			},
		},
		{
			name:   "known and missing known",
			encode: []tlv.Type{knownType},
			decode: []tlv.Type{knownType, secondKnownType},
			expParsedTypes: tlv.TypeMap{
				knownType: nil,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testParsedTypes(t, test)
		})
	}
}

func testParsedTypes(t *testing.T, test parsedTypeTest) {
	encRecords := make([]tlv.Record, 0, len(test.encode))
	for _, typ := range test.encode {
		encRecords = append(
			encRecords, tlv.MakePrimitiveRecord(typ, new(uint64)),
		)
	}

	decRecords := make([]tlv.Record, 0, len(test.decode))
	for _, typ := range test.decode {
		decRecords = append(
			decRecords, tlv.MakePrimitiveRecord(typ, new(uint64)),
		)
	}

	// Construct a stream that will encode the test's set of types.
	encStream := tlv.MustNewStream(encRecords...)

	var b bytes.Buffer
	if err := encStream.Encode(&b); err != nil {
		t.Fatalf("unable to encode stream: %v", err)
	}

	// Create a stream that will parse a subset of the test's types.
	decStream := tlv.MustNewStream(decRecords...)

	parsedTypes, err := decStream.DecodeWithParsedTypes(
		bytes.NewReader(b.Bytes()),
	)
	if err != nil {
		t.Fatalf("error decoding: %v", err)
	}
	if !reflect.DeepEqual(parsedTypes, test.expParsedTypes) {
		t.Fatalf("error mismatch on parsed types")
	}
}

var (
	smallValue      = 1
	smallValueBytes = []byte{
		// uint32 tlv, value uses 1 byte.
		0xa, 0x1, 0x1,
		// uint64 tlv, value uses 1 byte.
		0xb, 0x1, 0x1,
	}

	medianValue      = 255
	medianValueBytes = []byte{
		// uint32 tlv, value uses 3 byte.
		0xa, 0x3, 0xfd, 0x0, 0xff,
		// uint64 tlv, value uses 3 byte.
		0xb, 0x3, 0xfd, 0x0, 0xff,
	}

	largeValue      = 65536
	largeValueBytes = []byte{
		// uint32 tlv, value uses 5 byte.
		0xa, 0x5, 0xfe, 0x0, 0x1, 0x0, 0x0,
		// uint64 tlv, value uses 5 byte.
		0xb, 0x5, 0xfe, 0x0, 0x1, 0x0, 0x0,
	}
)

// TestEncodeBigSizeFormatTlvStream tests that the bigsize encoder works as
// expected.
func TestEncodeBigSizeFormatTlvStream(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		value         int
		expectedBytes []byte
	}{
		{
			// Test encode 1, which saves us space.
			name:          "encode small value",
			value:         smallValue,
			expectedBytes: smallValueBytes,
		},
		{
			// Test encode 255, which still saves us space.
			name:          "encode median value",
			value:         medianValue,
			expectedBytes: medianValueBytes,
		},
		{
			// Test encode 65536, which takes more space to encode
			// an uint32.
			name:          "encode large value",
			value:         largeValue,
			expectedBytes: largeValueBytes,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			testUint32 := uint32(tc.value)
			testUint64 := uint64(tc.value)
			ts := makeBigSizeFormatTlvStream(
				t, &testUint32, &testUint64,
			)

			// Encode the tlv stream.
			buf := bytes.NewBuffer([]byte{})
			require.NoError(t, ts.Encode(buf))

			// Check the bytes are written as expected.
			require.Equal(t, tc.expectedBytes, buf.Bytes())
		})
	}
}

// TestDecodeBigSizeFormatTlvStream tests that the bigsize decoder works as
// expected.
func TestDecodeBigSizeFormatTlvStream(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		bytes         []byte
		expectedValue int
	}{
		{
			// Test decode 1.
			name: "decode small value",
			bytes: []byte{
				// uint32 tlv, value uses 1 byte.
				0xa, 0x1, 0x1,
				// uint64 tlv, value uses 1 byte.
				0xb, 0x1, 0x1,
			},
			expectedValue: smallValue,
		},
		{
			// Test decode 255.
			name: "decode median value",
			bytes: []byte{
				// uint32 tlv, value uses 3 byte.
				0xa, 0x3, 0xfd, 0x0, 0xff,
				// uint64 tlv, value uses 3 byte.
				0xb, 0x3, 0xfd, 0x0, 0xff,
			},
			expectedValue: medianValue,
		},
		{
			// Test decode 65536.
			name: "decode value 65536",
			bytes: []byte{
				// uint32 tlv, value uses 5 byte.
				0xa, 0x5, 0xfe, 0x0, 0x1, 0x0, 0x0,
				// uint64 tlv, value uses 5 byte.
				0xb, 0x5, 0xfe, 0x0, 0x1, 0x0, 0x0,
			},
			expectedValue: largeValue,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var (
				testUint32 uint32
				testUint64 uint64
			)
			ts := makeBigSizeFormatTlvStream(
				t, &testUint32, &testUint64,
			)

			// Decode the tlv stream.
			buf := bytes.NewBuffer(tc.bytes)
			require.NoError(t, ts.Decode(buf))

			// Check the values are written as expected.
			require.EqualValues(t, tc.expectedValue, testUint32)
			require.EqualValues(t, tc.expectedValue, testUint64)
		})
	}
}

func makeBigSizeFormatTlvStream(t *testing.T, vUint32 *uint32,
	vUint64 *uint64) *tlv.Stream {

	const (
		typeUint32 tlv.Type = 10
		typeUint64 tlv.Type = 11
	)

	// Create a dummy tlv stream for testing.
	ts, err := tlv.NewStream(
		tlv.MakeBigSizeRecord(typeUint32, vUint32),
		tlv.MakeBigSizeRecord(typeUint64, vUint64),
	)
	require.NoError(t, err)

	return ts
}
