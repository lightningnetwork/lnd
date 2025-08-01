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

// TestDecodeDNSHostnameAddress ensures correct decoding of DNS net address from
// its binary representation if it was originally under opaque address type.
func TestDecodeDNSHostnameAddress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		data      []byte
		expected  net.Addr
		expectErr bool
		errMsg    string
	}{
		{
			name: "ValidAddressDecodesCorrectly",
			data: []byte{
				byte(opaqueAddrs),
				0x00, 0x0D, // payload length: 13
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x26, 0x07, // port 9735 in big-endian
			},
			expected: &lnwire.DNSAddr{
				Hostname: "example.com",
				Port:     9735,
			},
			expectErr: false,
		},
		{
			name: "AddressWithNonASCIICharacterReturnsOpaqueAddr",
			data: []byte{
				byte(opaqueAddrs),
				0x00, 0x0D, // payload length: 13
				'é', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x26, 0x07, // port 9735 in big-endian
			},
			expected: &lnwire.OpaqueAddrs{
				Payload: []uint8{
					0xe9, 0x78, 0x61, 0x6d, 0x70, 0x6c,
					0x65, 0x2e, 0x63, 0x6f, 0x6d, 0x26,
					0x7,
				},
			},
		},
		{
			name: "ValidAddressWithNonStandardPortDecodesCorrectly",
			data: []byte{
				byte(opaqueAddrs),
				0x00, 0x13, // payload length: 19
				'l', 'i', 'g', 'h', 't', 'n', 'i', 'n', 'g',
				'.', 'n', 'e', 't', 'w', 'o', 'r', 'k',
				0x04, 0xD2, // port 1234 in big-endian
			},
			expected: &lnwire.DNSAddr{
				Hostname: "lightning.network",
				Port:     1234,
			},
			expectErr: false,
		},
		{
			name:      "EmptyDataReturnsError",
			data:      []byte{},
			expectErr: true,
			errMsg:    "EOF",
		},
		{
			name:      "IncompleteDataWithoutLengthReturnsError",
			data:      []byte{byte(opaqueAddrs)},
			expectErr: true,
			errMsg:    "EOF",
		},
		{
			name: "IncompleteDataWithoutHostnameReturnsError",
			data: []byte{
				byte(opaqueAddrs),
				0x00, 0x0D, // Length of host that isn't there
			},
			expectErr: true,
			errMsg:    "EOF",
		},
		{
			name: "IncompleteDataWithoutPortReturnsError",
			data: []byte{
				byte(opaqueAddrs),
				0x00, 0x0D, // payload length: 13
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				// Missing port
			},
			expectErr: true,
			errMsg: "expected to read 13 bytes for payload, " +
				"but got 11 bytes",
		},
		{
			name: "IncompleteDataWithPartialHostnameReturnsError",
			data: []byte{
				byte(opaqueAddrs),
				0x00, 0x0D, // payload length: 13
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c',
				0x26, 0x07,
			},
			expectErr: true,
			errMsg: "expected to read 13 bytes for payload, " +
				"but got 11 bytes",
		},
		{
			name: "IncompleteDataWithPartialPortReturnsError",
			data: []byte{
				byte(opaqueAddrs),
				0x00, 0x0D, // payload length: 13
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x2, // Only 1 byte of port
			},
			expectErr: true,
			errMsg: "expected to read 13 bytes for payload, " +
				"but got 12 bytes",
		},
		{
			name: "ExcessiveDataDecodesCorrectlyAndIgnoresExcess",
			data: []byte{
				byte(opaqueAddrs),
				0x00, 0x0D, // payload length: 13
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x26, 0x07,
				// Extra data
				0x01, 0x02, 0x03,
			},
			expected: &lnwire.DNSAddr{
				Hostname: "example.com",
				Port:     9735,
			},
			expectErr: false,
		},
		{
			name: "UnknownAddressTypeReturnsError",
			data: []byte{
				byte(0xFF),
				0x00, 0x0D, // payload length: 13
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x26, 0x07, // port 9735 in big-endian
			},
			expected: &lnwire.DNSAddr{
				Hostname: "example.com",
				Port:     9735,
			},
			expectErr: true,
			errMsg:    ErrUnknownAddressType.Error(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := bytes.NewReader(tc.data)
			addr, err := DeserializeAddr(r)
			if tc.expectErr {
				require.ErrorContains(t, err, tc.errMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, addr)
		})
	}
}
