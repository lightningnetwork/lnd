package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestCloseCompleteEncodeDecode(t *testing.T) {
	cc := &CloseComplete{
		ChannelID:         uint64(12345678),
		ResponderCloseSig: commitSig,
	}

	// Next encode the CC message into an empty bytes buffer.
	var b bytes.Buffer
	if err := cc.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode CloseComplete: %v", err)
	}

	// Deserialize the encoded CC message into a new empty struct.
	cc2 := &CloseComplete{}
	if err := cc2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode CloseComplete: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(cc, cc2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			cc, cc2)
	}
}
