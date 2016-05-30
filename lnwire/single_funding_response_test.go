package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestSingleFundingResponseWire(t *testing.T) {
	// First create a new SFR message.
	var rev [20]byte
	cdp := pubKey
	delivery := PkScript(bytes.Repeat([]byte{0x02}, 25))
	sfr := NewSingleFundingResponse(22, rev, cdp, delivery)

	// Next encode the SFR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := sfr.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode SingleFundingSignComplete: %v", err)
	}

	// Deserialize the encoded SFR message into a new empty struct.
	sfr2 := &SingleFundingResponse{}
	if err := sfr2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode SingleFundingResponse: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(sfr, sfr2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			sfr, sfr2)
	}
}
