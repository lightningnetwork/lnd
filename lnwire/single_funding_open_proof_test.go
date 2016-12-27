package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestSingleFundingOpenProofWire(t *testing.T) {
	// First create a new SFOP message.
	sfop := NewSingleFundingOpenProof(22, someChannelID)

	// Next encode the SFOP message into an empty bytes buffer.
	var b bytes.Buffer
	if err := sfop.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode SingleFundingSignComplete: %v", err)
	}

	// Deserialize the encoded SFOP message into a new empty struct.
	sfop2 := &SingleFundingOpenProof{}
	if err := sfop2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode SingleFundingOpenProof: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(sfop, sfop2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			sfop, sfop2)
	}
}
