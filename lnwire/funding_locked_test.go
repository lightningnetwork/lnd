package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestFundingLockedWire(t *testing.T) {
	// First create a new FundingLocked message.
	fl := NewFundingLocked(ChannelID(revHash), pubKey)

	// Next encode the FundingLocked message into an empty bytes buffer.
	var b bytes.Buffer
	if err := fl.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode FundingLocked: %v", err)
	}

	// Check to ensure that the FundingLocked message is the correct size.
	if uint32(b.Len()) > fl.MaxPayloadLength(0) {
		t.Fatalf("length of FundingLocked message is too long: %v should be less than %v",
			b.Len(), fl.MaxPayloadLength(0))
	}

	// Deserialize the encoded FundingLocked message into an empty struct.
	fl2 := &FundingLocked{}
	if err := fl2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode FundingLocked: %v", err)
	}

	// Assert equality of the two instances
	if !reflect.DeepEqual(fl, fl2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v", fl, fl2)
	}
}
