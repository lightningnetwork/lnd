package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestPongEncodeDecode(t *testing.T) {
	pong := &Pong{
		PongBytes: bytes.Repeat([]byte("A"), 100),
	}

	// Next encode the pong message into an empty bytes buffer.
	var b bytes.Buffer
	if err := pong.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode pong: %v", err)
	}

	// Deserialize the encoded pong message into a new empty struct.
	pong2 := &Pong{}
	if err := pong2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode ping: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(pong, pong2) {
		t.Fatalf("encode/decode pong messages don't match %#v vs %#v",
			pong, pong2)
	}
}
