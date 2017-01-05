package elkrem

import (
	"testing"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// TestElkremBig tries 10K hashes
func TestElkremBig(t *testing.T) {
	var rcv ElkremReceiver

	sndr := NewElkremSender(chainhash.DoubleHashH([]byte("elktest")))

	for n := uint64(0); n < 10000; n++ {
		sha, err := sndr.AtIndex(n)
		if err != nil {
			t.Fatal(err)
		}

		if err = rcv.AddNext(sha); err != nil {
			t.Fatal(err)
		}
	}

	ReceiverSerdesTest(t, &rcv)

	for n := uint64(0); n < 10000; n += 500 {
		if _, err := rcv.AtIndex(n); err != nil {
			t.Fatal(err)
		}
	}
}

// TestElkremLess tries 10K hashes
func TestElkremLess(t *testing.T) {
	var rcv ElkremReceiver

	sndr := NewElkremSender(chainhash.DoubleHashH([]byte("elktest2")))

	for n := uint64(0); n < 5000; n++ {
		sha, err := sndr.AtIndex(n)
		if err != nil {
			t.Fatal(err)
		}

		if err = rcv.AddNext(sha); err != nil {
			t.Fatal(err)
		}
	}

	for n := uint64(0); n < 5000; n += 500 {
		if _, err := rcv.AtIndex(n); err != nil {
			t.Fatal(err)
		}
	}
}
