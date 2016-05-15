package elkrem

import (
	"testing"

	"github.com/roasbeef/btcd/wire"
)

// TestElkremBig makes a height 63 (max size possible) tree and tries 10K hashes
func TestElkremBig(t *testing.T) {
	sndr := NewElkremSender(63, wire.DoubleSha256SH([]byte("elktest")))
	rcv := NewElkremReceiver(63)
	SenderSerdesTest(t, &sndr)
	for n := uint64(0); n < 10000; n++ {
		sha, err := sndr.Next()
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
	SenderSerdesTest(t, &sndr)
	ReceiverSerdesTest(t, &rcv)
	for n := uint64(0); n < 10000; n += 500 {
		sha, err := rcv.AtIndex(n)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Retreived index %d %s\n", n, sha.String())
	}
}

// TestElkremSmall makes a height 15 (65534 size) tree and tries 10K hashes
func TestElkremSmall(t *testing.T) {
	sndr := NewElkremSender(15, wire.DoubleSha256SH([]byte("elktest")))
	rcv := NewElkremReceiver(15)
	for n := uint64(0); n < 5000; n++ {
		sha, err := sndr.Next()
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
