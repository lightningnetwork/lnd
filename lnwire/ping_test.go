package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestPingEncodeDecode(t *testing.T) {
	ping := &Ping{
		NumPongBytes: 10,
		PaddingBytes: bytes.Repeat([]byte("A"), 100),
	}

	// Next encode the ping message into an empty bytes buffer.
	var b bytes.Buffer
	if err := ping.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode ping: %v", err)
	}

	// Deserialize the encoded ping message into a new empty struct.
	ping2 := &Ping{}
	if err := ping2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode ping: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(ping, ping2) {
		t.Fatalf("encode/decode ping messages don't match %#v vs %#v",
			ping, ping2)
	}
}
