package tlv_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
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
