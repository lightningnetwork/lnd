package lnwire

import (
	"bytes"
	"math/big"
	"reflect"
	"testing"

	"github.com/roasbeef/btcd/btcec"
)

func TestSingleFundingSignCompleteWire(t *testing.T) {
	// First create a new SFSC message.
	sfsc := NewSingleFundingSignComplete(
		revHash,
		&btcec.Signature{
			R: new(big.Int).SetInt64(9),
			S: new(big.Int).SetInt64(11),
		},
	)

	// Next encode the SFSC message into an empty bytes buffer.
	var b bytes.Buffer
	if err := sfsc.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode SingleFundingSignComplete: %v", err)
	}

	// Deserialize the encoded SFSC message into a new empty struct.
	sfsc2 := &SingleFundingSignComplete{}
	if err := sfsc2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode SingleFundingSignComplete: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(sfsc, sfsc2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			sfsc, sfsc2)
	}
}
