package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestNodeAnnouncementEncodeDecode(t *testing.T) {
	na := &NodeAnnouncement{
		Signature: someSig,
		Timestamp: maxUint32,
		NodeID:    pubKey,
		RGBColor:  someRGB,
		Alias:     someAlias,
		Addresses: someAddresses,
		Features:  someFeatures,
	}

	// Next encode the NA message into an empty bytes buffer.
	var b bytes.Buffer
	if err := na.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode NodeAnnouncement: %v", err)
	}

	// Deserialize the encoded NA message into a new empty struct.
	na2 := &NodeAnnouncement{}
	if err := na2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode NodeAnnouncement: %v", err)
	}

	// We do not encode the feature map in feature vector, for that reason
	// the node announcement messages will differ. Set feature map with nil
	// in order to use deep equal function.
	na.Features.featuresMap = nil

	// Assert equality of the two instances.
	if !reflect.DeepEqual(na, na2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			na, na2)
	}
}

func TestNodeAnnoucementPayloadLength(t *testing.T) {
	na := &NodeAnnouncement{
		Signature: someSig,
		Timestamp: maxUint32,
		NodeID:    pubKey,
		RGBColor:  someRGB,
		Alias:     someAlias,
		Addresses: someAddresses,
		Features:  someFeatures,
	}

	var b bytes.Buffer
	if err := na.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode node: %v", err)
	}

	serializedLength := uint32(b.Len())
	if serializedLength != 167 {
		t.Fatalf("payload length estimate is incorrect: expected %v "+
			"got %v", 167, serializedLength)
	}

	if na.MaxPayloadLength(0) != 8192 {
		t.Fatalf("max payload length doesn't match: expected 8192, got %v",
			na.MaxPayloadLength(0))
	}
}

func TestValidateAlias(t *testing.T) {
	if err := someAlias.Validate(); err != nil {
		t.Fatalf("alias was invalid: %v", err)
	}
}
