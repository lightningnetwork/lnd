package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestCommitSigEncodeDecode(t *testing.T) {
	commitSignature := &CommitSig{
		ChanID:    ChannelID(revHash),
		CommitSig: commitSig,
	}

	// Next encode the CS message into an empty bytes buffer.
	var b bytes.Buffer
	if err := commitSignature.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode CommitSig: %v", err)
	}

	// Deserialize the encoded EG message into a new empty struct.
	commitSignature2 := &CommitSig{}
	if err := commitSignature2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode CommitSig: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(commitSignature, commitSignature2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			commitSignature, commitSignature2)
	}
}
