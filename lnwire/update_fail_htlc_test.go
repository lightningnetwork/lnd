package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestUpdateFailHTLC(t *testing.T) {
	// First create a new UFH message.
	cancelMsg := &UpdateFailHTLC{
		ChannelPoint: *outpoint1,
		ID:           22,
	}
	cancelMsg.Reason = []byte{byte(UnknownDestination)}

	// Next encode the UFH message into an empty bytes buffer.
	var b bytes.Buffer
	if err := cancelMsg.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode CancelHTLC: %v", err)
	}

	// Deserialize the encoded UFH message into a new empty struct.
	cancelMsg2 := &UpdateFailHTLC{}
	if err := cancelMsg2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode CancelHTLC: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(cancelMsg, cancelMsg2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			cancelMsg, cancelMsg2)
	}
}
