package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestErrorGenericEncodeDecode(t *testing.T) {
	eg := &ErrorGeneric{
		ChannelPoint: outpoint1,
		ErrorID:      99,
		Problem:      "Hello world!",
	}

	// Next encode the EG message into an empty bytes buffer.
	var b bytes.Buffer
	if err := eg.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode ErrorGeneric: %v", err)
	}

	// Deserialize the encoded EG message into a new empty struct.
	eg2 := &ErrorGeneric{}
	if err := eg2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode ErrorGeneric: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(eg, eg2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			eg, eg2)
	}
}
