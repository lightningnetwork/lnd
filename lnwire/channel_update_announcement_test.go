package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestChannelUpdateAnnouncementEncodeDecode(t *testing.T) {
	cua := &ChannelUpdateAnnouncement{
		Signature:                 someSig,
		ShortChannelID:            someChannelID,
		Timestamp:                 maxUint32,
		Flags:                     maxUint16,
		TimeLockDelta:             maxUint16,
		HtlcMinimumMsat:           maxUint32,
		FeeBaseMsat:               maxUint32,
		FeeProportionalMillionths: maxUint32,
	}

	// Next encode the CUA message into an empty bytes buffer.
	var b bytes.Buffer
	if err := cua.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode ChannelUpdateAnnouncement: %v", err)
	}

	// Ensure the max payload estimate is correct.
	serializedLength := uint32(b.Len())
	if serializedLength != cua.MaxPayloadLength(0) {
		t.Fatalf("payload length estimate is incorrect: expected %v "+
			"got %v", serializedLength, cua.MaxPayloadLength(0))
	}

	// Deserialize the encoded CUA message into a new empty struct.
	cua2 := &ChannelUpdateAnnouncement{}
	if err := cua2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode ChannelUpdateAnnouncement: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(cua, cua2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			cua, cua2)
	}
}
