package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestHTLCAddRejectEncodeDecode(t *testing.T) {
	// First create a new HTLCAR message.
	rejectReq := &HTLCAddReject{
		ChannelPoint: outpoint1,
		HTLCKey:      22,
	}

	// Next encode the HTLCAR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := rejectReq.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode HTLCSettleRequest: %v", err)
	}

	// Deserialize the encoded HTLCAR message into a new empty struct.
	rejectReq2 := &HTLCAddReject{}
	if err := rejectReq2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode HTLCAddReject: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(rejectReq, rejectReq2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			rejectReq, rejectReq2)
	}
}
