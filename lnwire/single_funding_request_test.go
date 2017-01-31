package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestSingleFundingRequestWire(t *testing.T) {
	// First create a new SFR message.
	cdp := pubKey
	delivery := PkScript(bytes.Repeat([]byte{0x02}, 25))
	sfr := NewSingleFundingRequest(20, 21, 22, 23, 5, 5, cdp, cdp,
		delivery, 540, 10000, 6)

	// Next encode the SFR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := sfr.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode SingleFundingRequest: %v", err)
	}

	// Deserialize the encoded SFR message into a new empty struct.
	sfr2 := &SingleFundingRequest{}
	if err := sfr2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode SingleFundingRequest: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(sfr, sfr2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			sfr, sfr2)
	}
}
