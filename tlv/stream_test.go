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
	require.NoError(t, err, "error decoding")
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

// TestDecodeP2P tests that the p2p variants of the stream decode functions
// work with small records and fail with large records.
func TestDecodeP2P(t *testing.T) {
	t.Parallel()

	const (
		smallType tlv.Type = 8
		largeType tlv.Type = 10
	)

	var (
		smallBytes = []byte{
			0x08, // tlv type = 8
			0x10, // length = 16
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
		}

		largeBytes = []byte{
			0x0a,                         // tlv type = 10
			0xfe, 0x00, 0x01, 0x00, 0x00, // length = 65536
		}
	)

	// Verify the expected behavior for the large type.
	s, err := tlv.NewStream(tlv.MakePrimitiveRecord(largeType, &[]byte{}))
	require.NoError(t, err)

	// Decoding with either of the p2p stream decoders should fail with the
	// record too large error.
	buf := bytes.NewBuffer(largeBytes)
	require.Equal(t, s.DecodeP2P(buf), tlv.ErrRecordTooLarge)

	buf2 := bytes.NewBuffer(largeBytes)
	_, err = s.DecodeWithParsedTypesP2P(buf2)
	require.Equal(t, err, tlv.ErrRecordTooLarge)

	// Extend largeBytes with a payload of 65536 bytes so that the non-p2p
	// decoders can successfully decode it.
	largeSlice := make([]byte, 65542)
	copy(largeSlice[:6], largeBytes)
	buf3 := bytes.NewBuffer(largeSlice)
	require.NoError(t, s.Decode(buf3))

	buf4 := bytes.NewBuffer(largeSlice)
	_, err = s.DecodeWithParsedTypes(buf4)
	require.NoError(t, err)

	// Now create a new stream and assert that the p2p-variants can decode
	// small types.
	s2, err := tlv.NewStream(tlv.MakePrimitiveRecord(smallType, &[]byte{}))
	require.NoError(t, err)

	buf5 := bytes.NewBuffer(smallBytes)
	require.NoError(t, s2.DecodeP2P(buf5))

	buf6 := bytes.NewBuffer(smallBytes)
	_, err = s2.DecodeWithParsedTypesP2P(buf6)
	require.NoError(t, err)
}
