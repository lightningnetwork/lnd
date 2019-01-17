package channeldb

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/tor"
)

type unknownAddrType struct{}

func (t unknownAddrType) Network() string { return "unknown" }
func (t unknownAddrType) String() string  { return "unknown" }

var testIP4 = net.ParseIP("192.168.1.1")
var testIP6 = net.ParseIP("2001:0db8:0000:0000:0000:ff00:0042:8329")

var addrTests = []struct {
	expAddr net.Addr
	serErr  string
}{
	// Valid addresses.
	{
		expAddr: &net.TCPAddr{
			IP:   testIP4,
			Port: 12345,
		},
	},
	{
		expAddr: &net.TCPAddr{
			IP:   testIP6,
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

	// Invalid addresses.
	{
		expAddr: unknownAddrType{},
		serErr:  ErrUnknownAddressType.Error(),
	},
	{
		expAddr: &net.TCPAddr{
			// Remove last byte of IPv4 address.
			IP:   testIP4[:len(testIP4)-1],
			Port: 12345,
		},
		serErr: "unable to encode",
	},
	{
		expAddr: &net.TCPAddr{
			// Add an extra byte of IPv4 address.
			IP:   append(testIP4, 0xff),
			Port: 12345,
		},
		serErr: "unable to encode",
	},
	{
		expAddr: &net.TCPAddr{
			// Remove last byte of IPv6 address.
			IP:   testIP6[:len(testIP6)-1],
			Port: 65535,
		},
		serErr: "unable to encode",
	},
	{
		expAddr: &net.TCPAddr{
			// Add an extra byte to the IPv6 address.
			IP:   append(testIP6, 0xff),
			Port: 65535,
		},
		serErr: "unable to encode",
	},
}

// TestAddrSerialization tests that the serialization method used by channeldb
// for net.Addr's works as intended.
func TestAddrSerialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	for _, test := range addrTests {
		err := serializeAddr(&b, test.expAddr)
		switch {
		case err == nil && test.serErr != "":
			t.Fatalf("expected serialization err for addr %v",
				test.expAddr)

		case err != nil && test.serErr == "":
			t.Fatalf("unexpected serialization err for addr %v: %v",
				test.expAddr, err)

		case err != nil && !strings.Contains(err.Error(), test.serErr):
			t.Fatalf("unexpected serialization err for addr %v, "+
				"want: %v, got %v", test.expAddr, test.serErr,
				err)

		case err != nil:
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
