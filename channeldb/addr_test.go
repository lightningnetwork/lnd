package channeldb

import (
	"bytes"
	"net"
	"testing"

	"github.com/lightningnetwork/lnd/tor"
)

type unknownAddrType struct{}

func (t unknownAddrType) Network() string { return "unknown" }
func (t unknownAddrType) String() string  { return "unknown" }

var addrTests = []struct {
	expAddr net.Addr
	serErr  error
}{
	{
		expAddr: &net.TCPAddr{
			IP:   net.ParseIP("192.168.1.1"),
			Port: 12345,
		},
	},
	{
		expAddr: &net.TCPAddr{
			IP:   net.ParseIP("2001:0db8:0000:0000:0000:ff00:0042:8329"),
			Port: 65535,
		},
	},
	{
		expAddr: &tor.OnionAddr{
			OnionService: "3g2upl4pq6kufc4m.onion",
			Port:         9735,
		},
	},
	{
		expAddr: &tor.OnionAddr{
			OnionService: "vww6ybal4bd7szmgncyruucpgfkqahzddi37ktceo3ah7ngmcopnpyyd.onion",
			Port:         80,
		},
	},
	{
		expAddr: unknownAddrType{},
		serErr:  ErrUnknownAddressType,
	},
}

// TestAddrSerialization tests that the serialization method used by channeldb
// for net.Addr's works as intended.
func TestAddrSerialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	for _, test := range addrTests {
		err := serializeAddr(&b, test.expAddr)
		if err != test.serErr {
			t.Fatalf("unexpected serialization err for addr %v, "+
				"want: %v, got %v",
				test.expAddr, test.serErr, err)
		} else if test.serErr != nil {
			continue
		}

		addr, err := deserializeAddr(&b)
		if err != nil {
			t.Fatalf("unable to deserialize address: %v", err)
		}

		if addr.String() != test.expAddr.String() {
			t.Fatalf("expected address %v after serialization, "+
				"got %v", addr, test.expAddr)
		}
	}
}
