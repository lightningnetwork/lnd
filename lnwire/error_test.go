package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestErrorEncodeDecode(t *testing.T) {
	eg := &Error{
		ChanID: ChannelID(revHash),
		Code:   99,
		Data:   []byte{'k', 'e', 'k'},
	}

	// Next encode the error message into an empty bytes buffer.
	var b bytes.Buffer
	if err := eg.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode ErrorGeneric: %v", err)
	}

	// Deserialize the encoded error message into a new empty struct.
	eg2 := &Error{}
	if err := eg2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode ErrorGeneric: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(eg, eg2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			eg, eg2)
	}
}
