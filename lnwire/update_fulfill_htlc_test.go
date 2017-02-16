package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestUpdateFufillHTLCEncodeDecode(t *testing.T) {
	// First create a new HTLCSR message.
	settleReq := NewUpdateFufillHTLC(*outpoint1, 23, revHash)

	// Next encode the HTLCSR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := settleReq.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode UpdateFufillHTLC: %v", err)
	}

	// Deserialize the encoded SFOP message into a new empty struct.
	settleReq2 := &UpdateFufillHTLC{}
	if err := settleReq2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode UpdateFufillHTLC: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(settleReq, settleReq2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			settleReq, settleReq2)
	}
}
