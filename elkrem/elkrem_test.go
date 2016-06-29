package elkrem

import (
	"testing"

	"github.com/roasbeef/btcd/wire"
)

// TestElkremBig tries 10K hashes
func TestElkremBig(t *testing.T) {
	sndr := NewElkremSender(wire.DoubleSha256SH([]byte("elktest")))
	var rcv ElkremReceiver
	//	SenderSerdesTest(t, sndr)
	for n := uint64(0); n < 10000; n++ {
		sha, err := sndr.AtIndex(n)
		if err != nil {
			t.Fatal(err)
		}
		err = rcv.AddNext(sha)
		if err != nil {
			t.Fatal(err)
		}
		if n%1000 == 999 {
			t.Logf("stack with %d received hashes\n", n+1)
			for i, n := range rcv.s {
				t.Logf("Stack element %d: index %d height %d %s\n",
					i, n.i, n.h, n.sha.String())
			}
		}
	}
	//	SenderSerdesTest(t, sndr)
	ReceiverSerdesTest(t, &rcv)
	for n := uint64(0); n < 10000; n += 500 {
		sha, err := rcv.AtIndex(n)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Retreived index %d %s\n", n, sha.String())
	}
}

// TestElkremLess tries 10K hashes
func TestElkremLess(t *testing.T) {
	sndr := NewElkremSender(wire.DoubleSha256SH([]byte("elktest2")))
	var rcv ElkremReceiver
	for n := uint64(0); n < 5000; n++ {
		sha, err := sndr.AtIndex(n)
		if err != nil {
			t.Fatal(err)
		}
		err = rcv.AddNext(sha)
		if err != nil {
			t.Fatal(err)
		}
		if n%1000 == 999 {
			t.Logf("stack with %d received hashes\n", n+1)
			for i, n := range rcv.s {
				t.Logf("Stack element %d: index %d height %d %s\n",
					i, n.i, n.h, n.sha.String())
			}
		}
	}
	for n := uint64(0); n < 5000; n += 500 {
		sha, err := rcv.AtIndex(n)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Retreived index %d %s\n",
			n, sha.String())
	}
}
