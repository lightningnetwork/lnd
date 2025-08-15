package graphdb

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

type unknownAddrType struct{}

func (t unknownAddrType) Network() string { return "unknown" }
func (t unknownAddrType) String() string  { return "unknown" }

var testIP4 = net.ParseIP("192.168.1.1").To4()
var testIP6 = net.ParseIP("2001:0db8:0000:0000:0000:ff00:0042:8329")

var (
	testIPV4Addr = &net.TCPAddr{
		IP:   testIP4,
		Port: 12345,
	}

	testIPV6Addr = &net.TCPAddr{
		IP:   testIP6,
		Port: 65535,
	}

	testOnionV2Addr = &tor.OnionAddr{
		OnionService: "3g2upl4pq6kufc4m.onion",
		Port:         9735,
	}

	testOnionV3Addr = &tor.OnionAddr{
		OnionService: "vww6ybal4bd7szmgncyruucpgfkqahzddi37ktceo3ah7ngmcopnpyyd.onion", //nolint:ll
		Port:         80,
	}

	testOpaqueAddr = &lnwire.OpaqueAddrs{
		// NOTE: the first byte is a protocol level address type. So
		// for we set it to 0xff to guarantee that we do not know this
		// type yet.
		Payload: []byte{0xff, 0x02, 0x03, 0x04, 0x05, 0x06},
	}

	testDNSAddr = &lnwire.DNSAddress{
		Hostname: "example.com",
		Port:     8080,
	}
)

var addrTests = []struct {
	expAddr net.Addr
	serErr  string
}{
	// Valid addresses.
	{
		expAddr: testIPV4Addr,
	},
	{
		expAddr: testIPV6Addr,
	},
	{
		expAddr: testOnionV2Addr,
	},
	{
		expAddr: testOnionV3Addr,
	},
	{
		expAddr: testOpaqueAddr,
	},
	{
		expAddr: testDNSAddr,
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
	{
		expAddr: &tor.OnionAddr{
			// Invalid suffix.
			OnionService: "vww6ybal4bd7szmgncyruucpgfkqahzddi37ktceo3ah7ngmcopnpyyd.inion",
			Port:         80,
		},
		serErr: "invalid suffix",
	},
	{
		expAddr: &tor.OnionAddr{
			// Invalid length.
			OnionService: "vww6ybal4bd7szmgncyruucpgfkqahzddi37ktceo3ah7ngmcopnpyy.onion",
			Port:         80,
		},
		serErr: "unknown onion service length",
	},
	{
		expAddr: &tor.OnionAddr{
			// Invalid encoding.
			OnionService: "vww6ybal4bd7szmgncyruucpgfkqahzddi37ktceo3ah7ngmcopnpyyA.onion",
			Port:         80,
		},
		serErr: "illegal base32",
	},
}

// TestAddrSerialization tests that the serialization method used by channeldb
// for net.Addr's works as intended.
func TestAddrSerialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	for _, test := range addrTests {
		err := SerializeAddr(&b, test.expAddr)
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

		addr, err := DeserializeAddr(&b)
		if err != nil {
			t.Fatalf("unable to deserialize address: %v", err)
		}

		require.Equal(t, test.expAddr, addr)
	}
}
