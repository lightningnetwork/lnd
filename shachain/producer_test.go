package shachain

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// TestShaChainProducerRestore checks the ability of shachain producer to be
// properly recreated from binary representation.
func TestShaChainProducerRestore(t *testing.T) {
	t.Parallel()

	var err error

	seed := chainhash.DoubleHashH([]byte("shachaintest"))
	sender := NewRevocationProducer(seed)

	s1, err := sender.AtIndex(0)
	if err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	if err := sender.Encode(&b); err != nil {
		t.Fatal(err)
	}

	sender, err = NewRevocationProducerFromBytes(b.Bytes())
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
