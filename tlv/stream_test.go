package tlv_test

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
)

// TestParsedTypes asserts that a Stream will properly return the set of types
// that it encounters when the type is known-and-decoded or unknown-and-ignored.
func TestParsedTypes(t *testing.T) {
	const (
		knownType   = 1
		unknownType = 3
	)

	// Construct a stream that will encode two types, one that will be known
	// to the decoder and another that will be unknown.
	encStream := tlv.MustNewStream(
		tlv.MakePrimitiveRecord(knownType, new(uint64)),
		tlv.MakePrimitiveRecord(unknownType, new(uint64)),
	)

	var b bytes.Buffer
	if err := encStream.Encode(&b); err != nil {
		t.Fatalf("unable to encode stream: %v", err)
	}

	// Create a stream that will parse only the known type.
	decStream := tlv.MustNewStream(
		tlv.MakePrimitiveRecord(knownType, new(uint64)),
	)

	parsedTypes, err := decStream.DecodeWithParsedTypes(
		bytes.NewReader(b.Bytes()),
	)
	if err != nil {
		t.Fatalf("unable to decode stream: %v", err)
	}

	// Assert that both the known and unknown types are included in the set
	// of parsed types.
	if _, ok := parsedTypes[knownType]; !ok {
		t.Fatalf("known type %d should be in parsed types", knownType)
	}
	if _, ok := parsedTypes[unknownType]; !ok {
		t.Fatalf("unknown type %d should be in parsed types",
			unknownType)
	}
}
