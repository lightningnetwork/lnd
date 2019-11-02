package tlv_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
)

type parsedTypeTest struct {
	name   string
	encode []tlv.Type
	decode []tlv.Type
	expErr error
}

// TestParsedTypes asserts that a Stream will properly return the set of types
// that it encounters when the type is known-and-decoded or unknown-and-ignored.
func TestParsedTypes(t *testing.T) {
	const (
		firstReqType  = 0
		knownType     = 1
		unknownType   = 3
		secondReqType = 4
	)

	tests := []parsedTypeTest{
		{
			name:   "known optional and unknown optional",
			encode: []tlv.Type{knownType, unknownType},
			decode: []tlv.Type{knownType},
		},
		{
			name:   "unknown required and known optional",
			encode: []tlv.Type{firstReqType, knownType},
			decode: []tlv.Type{knownType},
			expErr: tlv.ErrUnknownRequiredType(firstReqType),
		},
		{
			name:   "unknown required and unknown optional",
			encode: []tlv.Type{unknownType, secondReqType},
			expErr: tlv.ErrUnknownRequiredType(secondReqType),
		},
		{
			name:   "unknown required and known required",
			encode: []tlv.Type{firstReqType, secondReqType},
			decode: []tlv.Type{secondReqType},
			expErr: tlv.ErrUnknownRequiredType(firstReqType),
		},
		{
			name:   "two unknown required",
			encode: []tlv.Type{firstReqType, secondReqType},
			expErr: tlv.ErrUnknownRequiredType(firstReqType),
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
	if !reflect.DeepEqual(err, test.expErr) {
		t.Fatalf("error mismatch, want: %v got: %v", err, test.expErr)
	}

	// Assert that all encoded types are included in the set of parsed
	// types.
	for _, typ := range test.encode {
		if _, ok := parsedTypes[typ]; !ok {
			t.Fatalf("encoded type %d should be in parsed types",
				typ)
		}
	}
}
