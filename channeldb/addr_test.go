package channeldb

import (
	"bytes"
	"net"
	"testing"

	"github.com/lightningnetwork/lnd/tor"
)

// TestAddrSerialization tests that the serialization method used by channeldb
// for net.Addr's works as intended.
func TestAddrSerialization(t *testing.T) {
	t.Parallel()

	testAddrs := []net.Addr{
		&net.TCPAddr{
			IP:   net.ParseIP("192.168.1.1"),
			Port: 12345,
		},
		&net.TCPAddr{
			IP:   net.ParseIP("2001:0db8:0000:0000:0000:ff00:0042:8329"),
			Port: 65535,
		},
		&tor.OnionAddr{
			OnionService: "3g2upl4pq6kufc4m.onion",
			Port:         9735,
		},
		&tor.OnionAddr{
			OnionService: "vww6ybal4bd7szmgncyruucpgfkqahzddi37ktceo3ah7ngmcopnpyyd.onion",
			Port:         80,
		},
	}

	var b bytes.Buffer
	for _, expectedAddr := range testAddrs {
		if err := serializeAddr(&b, expectedAddr); err != nil {
			t.Fatalf("unable to serialize address: %v", err)
		}

		addr, err := deserializeAddr(&b)
		if err != nil {
			t.Fatalf("unable to deserialize address: %v", err)
		}

		if addr.String() != expectedAddr.String() {
			t.Fatalf("expected address %v after serialization, "+
				"got %v", addr, expectedAddr)
		}
	}
}
