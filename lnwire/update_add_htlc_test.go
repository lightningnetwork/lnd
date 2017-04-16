package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/roasbeef/btcutil"
)

func TestUpdateAddHTLCEncodeDecode(t *testing.T) {
	// First create a new UPAH message.
	addReq := &UpdateAddHTLC{
		ChanID:      ChannelID(revHash),
		ID:          99,
		Expiry:      uint32(144),
		Amount:      btcutil.Amount(123456000),
		PaymentHash: revHash,
	}
	copy(addReq.OnionBlob[:], bytes.Repeat([]byte{23}, OnionPacketSize))

	// Next encode the HTLCAR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := addReq.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode HTLCAddRequest: %v", err)
	}

	// Deserialize the encoded UPAH message into a new empty struct.
	addReq2 := &UpdateAddHTLC{}
	if err := addReq2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode HTLCAddRequest: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(addReq, addReq2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			addReq, addReq2)
	}
}
