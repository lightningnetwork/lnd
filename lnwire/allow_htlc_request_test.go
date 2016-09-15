package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestAllowHTLCRequestEncodeDecode(t *testing.T) {
	msg := &AllowHTLCRequestMessage{
		PartnerID: "some partner id",
		RequestID: 121,
		Amount: 1001,
	}

	// Next encode the CC message into an empty bytes buffer.
	var b bytes.Buffer
	if err := msg.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode AllowHTLCRequest: %v", err)
	}

	// Deserialize the encoded CC message into a new empty struct.
	msgDecoded := &AllowHTLCRequestMessage{}
	if err := msgDecoded.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode AllowHTLCRequest: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(msg, msgDecoded) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			msg, msgDecoded)
	}
}
