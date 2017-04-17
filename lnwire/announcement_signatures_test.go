package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestAnnounceSignatureEncodeDecode(t *testing.T) {
	ac := &AnnounceSignatures{
		ChannelID:        ChannelID(revHash),
		ShortChannelID:   NewShortChanIDFromInt(1),
		NodeSignature:    someSig,
		BitcoinSignature: someSig,
	}

	// Next encode the message into an empty bytes buffer.
	var b bytes.Buffer
	if err := ac.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode AnnounceSignatures: %v", err)
	}

	// Deserialize the encoded message into a new empty struct.
	ac2 := &AnnounceSignatures{}
	if err := ac2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode AnnounceSignatures: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(ac, ac2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			ac, ac2)
	}
}
