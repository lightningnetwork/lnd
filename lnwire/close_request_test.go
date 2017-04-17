package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/roasbeef/btcutil"
)

func TestCloseRequestEncodeDecode(t *testing.T) {
	cr := &CloseRequest{
		ChanID:            ChannelID(revHash),
		RequesterCloseSig: commitSig,
		Fee:               btcutil.Amount(10000),
	}

	// Next encode the CR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := cr.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode CloseRequest: %v", err)
	}

	// Deserialize the encoded CR message into a new empty struct.
	cr2 := &CloseRequest{}
	if err := cr2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode CloseRequest: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(cr, cr2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			cr, cr2)
	}
}
