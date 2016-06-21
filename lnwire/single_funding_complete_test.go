package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestSingleFundingCompleteWire(t *testing.T) {
	// First create a new SFC message.
	sfc := NewSingleFundingComplete(22, outpoint1, commitSig1)

	// Next encode the SFC message into an empty bytes buffer.
	var b bytes.Buffer
	if err := sfc.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode SingleFundingComplete: %v", err)
	}

	// Deserialize the encoded SFC message into a new empty struct.
	sfc2 := &SingleFundingComplete{}
	if err := sfc2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode SingleFundingComplete: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(sfc, sfc2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			sfc, sfc2)
	}
}
