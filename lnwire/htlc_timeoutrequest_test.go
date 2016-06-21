package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestHTLCTimeoutRequestEncodeDecode(t *testing.T) {
	// First create a new HTLCTR message.
	timeoutReq := &HTLCTimeoutRequest{
		ChannelPoint: outpoint1,
		HTLCKey:      22,
	}

	// Next encode the HTLCTR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := timeoutReq.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode HTLCTimeoutRequest: %v", err)
	}

	// Deserialize the encoded HTLCTR message into a new empty struct.
	timeoutReq2 := &HTLCTimeoutRequest{}
	if err := timeoutReq2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode HTLCTimeoutRequest: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(timeoutReq, timeoutReq2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			timeoutReq, timeoutReq2)
	}
}
