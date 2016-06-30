package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestHTLCSettleRequestEncodeDecode(t *testing.T) {
	redemptionProofs := make([][32]byte, 1)
	redemptionProofs[0] = revHash

	// First create a new HTLCSR message.
	settleReq := NewHTLCSettleRequest(outpoint1, HTLCKey(23), redemptionProofs)

	// Next encode the HTLCSR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := settleReq.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode HTLCSettleRequest: %v", err)
	}

	// Deserialize the encoded SFOP message into a new empty struct.
	settleReq2 := &HTLCSettleRequest{}
	if err := settleReq2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode HTLCSettleRequest: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(settleReq, settleReq2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			settleReq, settleReq2)
	}
}
