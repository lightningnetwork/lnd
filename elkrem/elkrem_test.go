package elkrem

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
)

// TestElkremBig makes a height 63 (max size possible) tree and tries 10K hashes
func TestElkremBig(t *testing.T) {
	ex := NewElkremSender(63, wire.DoubleSha256SH([]byte("elktest")))
	rcv := NewElkremReceiver(63)
	for n := uint64(0); n < 10000; n++ {
		sha, err := ex.AtIndex(n)
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
	for n := uint64(0); n < 10000; n += 500 {
		sha, err := rcv.AtIndex(n)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Retreived index %d %s\n",
			n, sha.String())
	}
}

// TestElkremSmall makes a height 15 (65534 size) tree and tries 10K hashes
func TestElkremSmall(t *testing.T) {
	ex := NewElkremSender(15, wire.DoubleSha256SH([]byte("elktest")))
	rcv := NewElkremReceiver(15)
	for n := uint64(0); n < 5000; n++ {
		sha, err := ex.AtIndex(n)
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
