package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestAllowHTLCResponseEncodeDecode(t *testing.T) {
	msg := &AllowHTLCResponseMessage{
		RequestID: 121,
		Status: AllowHTLCStatus_Allow,
	}

	// Next encode the CC message into an empty bytes buffer.
	var b bytes.Buffer
	if err := msg.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode AllowHTLCResponse: %v", err)
	}

	// Deserialize the encoded CC message into a new empty struct.
	msgDecoded := &AllowHTLCResponseMessage{}
	if err := msgDecoded.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode AllowHTLCResponse: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(msg, msgDecoded) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			msg, msgDecoded)
	}
}
