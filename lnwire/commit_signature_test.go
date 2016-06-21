package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/roasbeef/btcutil"
)

func TestCommitSignatureEncodeDecode(t *testing.T) {
	copy(revocationHash[:], revocationHashBytes)

	commitSignature := &CommitSignature{
		ChannelPoint: outpoint1,
		Fee:          btcutil.Amount(10000),
		CommitSig:    commitSig,
	}

	// Next encode the CS message into an empty bytes buffer.
	var b bytes.Buffer
	if err := commitSignature.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode CommitSignature: %v", err)
	}

	// Deserialize the encoded EG message into a new empty struct.
	commitSignature2 := &CommitSignature{}
	if err := commitSignature2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode CommitSignature: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(commitSignature, commitSignature2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			commitSignature, commitSignature2)
	}
}
