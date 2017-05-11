package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

func TestOutpointSerialization(t *testing.T) {
	outpoint := wire.OutPoint{
		Hash: [chainhash.HashSize]byte{
			0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
			0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
			0x2d, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
			0x1f, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
		},
		Index: 9,
	}

	var buf bytes.Buffer

	if err := WriteOutPoint(&buf, &outpoint); err != nil {
		t.Fatalf("unable to serialize outpoint: %v", err)
	}

	var deserializedOutpoint wire.OutPoint
	if err := ReadOutPoint(&buf, &deserializedOutpoint); err != nil {
		t.Fatalf("unable to deserialize outpoint: %v", err)
	}

	if !reflect.DeepEqual(outpoint, deserializedOutpoint) {
		t.Fatalf("original and deserialized outpoints are different:\n"+
			"original     : %+v\n"+
			"deserialized : %+v\n",
			outpoint, deserializedOutpoint)
	}
}
