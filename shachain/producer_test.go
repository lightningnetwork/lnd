package shachain

import (
	"testing"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// TestShaChainProducerRestore checks the ability of shachain producer to be
// properly recreated from binary representation.
func TestShaChainProducerRestore(t *testing.T) {
	var err error

	hash := chainhash.DoubleHashH([]byte("shachaintest"))
	seed := &hash
	sender := NewRevocationProducer(seed)

	s1, err := sender.AtIndex(0)
	if err != nil {
		t.Fatal(err)
	}

	data, err := sender.ToBytes()
	if err != nil {
		t.Fatal(err)
	}

	sender, err = NewRevocationProducerFromBytes(data)
	if err != nil {
		t.Fatal(err)
	}

	s3, err := sender.AtIndex(0)
	if err != nil {
		t.Fatal(err)
	}

	if !s1.IsEqual(s3) {
		t.Fatalf("secrets should match: %v:%v", s1.String(), s3.String())
	}
}
