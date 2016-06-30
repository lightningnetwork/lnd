package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestCommitRevocationEncodeDecode(t *testing.T) {
	cr := &CommitRevocation{
		ChannelPoint:       outpoint1,
		Revocation:         revHash,
		NextRevocationKey:  pubKey,
		NextRevocationHash: revHash,
	}

	// Next encode the CR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := cr.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode CommitRevocation: %v", err)
	}

	// Deserialize the encoded EG message into a new empty struct.
	cr2 := &CommitRevocation{}
	if err := cr2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode CommitRevocation: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(cr, cr2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			cr, cr2)
	}
}
