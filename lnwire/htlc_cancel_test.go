package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestCancelHTLCEncodeDecode(t *testing.T) {
	// First create a new HTLCTR message.
	cancelMsg := &CancelHTLC{
		ChannelPoint: outpoint1,
		HTLCKey:      22,
		Reason:       UpstreamTimeout,
	}

	// Next encode the HTLCTR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := cancelMsg.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode CancelHTLC: %v", err)
	}

	// Deserialize the encoded HTLCTR message into a new empty struct.
	cancelMsg2 := &CancelHTLC{}
	if err := cancelMsg2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode CancelHTLC: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(cancelMsg, cancelMsg2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			cancelMsg, cancelMsg2)
	}
}
