package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestHTLCAddRequestEncodeDecode(t *testing.T) {
	redemptionHashes := make([][20]byte, 1)
	copy(redemptionHashes[0][:], bytes.Repeat([]byte{0x09}, 20))

	// First create a new HTLCAR message.
	addReq := &HTLCAddRequest{
		ChannelPoint:     outpoint1,
		Expiry:           uint32(144),
		Amount:           CreditsAmount(123456000),
		ContractType:     uint8(17),
		RedemptionHashes: redemptionHashes,
		OnionBlob:        []byte{255, 0, 255, 0, 255, 0, 255, 0},
	}

	// Next encode the HTLCAR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := addReq.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode HTLCAddRequest: %v", err)
	}

	// Deserialize the encoded HTLCAR message into a new empty struct.
	addReq2 := &HTLCAddRequest{}
	if err := addReq2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode HTLCAddRequest: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(addReq, addReq2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			addReq, addReq2)
	}
}
