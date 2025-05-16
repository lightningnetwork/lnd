package graphdb

import (
	"bytes"
	"errors"
	"io"
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
	{
		expAddr: &lnwire.DNSHostnameAddress{
			Hostname: "www.example.com",
			Port:     80,
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
	{
		expAddr: &lnwire.DNSHostnameAddress{
			// Invalid hostname length.
			Hostname: strings.Repeat("a", 252) + ".com",
			Port:     80,
		},
		serErr: "exceeds maximum length of 255 characters",
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

		if addr.String() != test.expAddr.String() {
			t.Fatalf("expected address %v after serialization, "+
				"got %v", addr, test.expAddr)
		}
	}
}

// TestEncodeDNSHostnameAddress verifies encoding of DNSHostnameAddress into
// its binary representation. It checks correct encoding for various valid
// cases, hostname length limits, and error propagation from the writer.
func TestEncodeDNSHostnameAddress(t *testing.T) {
	tests := []struct {
		name      string
		addr      *lnwire.DNSHostnameAddress
		writer    io.Writer
		expected  []byte
		expectErr bool
		errMsg    string
	}{
		{
			name: "ValidHostname ShortLength EncodesCorrectly",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &bytes.Buffer{},
			expected: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x26, 0x07, // port 9735 in big-endian
			},
			expectErr: false,
		},
		{
			name: "ValidHostname MaxLabelLength EncodesCorrectly",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: strings.Repeat("a", 63),
				Port:     8080,
			},
			writer: &bytes.Buffer{},
			expected: append(
				append(
					[]byte{byte(dnsHostnameAddr), 63},
					[]byte(strings.Repeat("a", 63))...,
				),
				[]byte{0x1F, 0x90}...,
			),
			expectErr: false,
		},
		{
			name: "ValidHostname NonStandardPort EncodesCorrectly",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "lightning.network",
				Port:     1234,
			},
			writer: &bytes.Buffer{},
			expected: []byte{
				byte(dnsHostnameAddr),
				17,
				'l', 'i', 'g', 'h', 't', 'n', 'i', 'n', 'g',
				'.', 'n', 'e', 't', 'w', 'o', 'r', 'k',
				0x04, 0xD2,
			},
			expectErr: false,
		},
		{
			name: "EmptyHostname ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "",
				Port:     9735,
			},
			writer:    &bytes.Buffer{},
			expected:  nil,
			expectErr: true,
			errMsg:    "hostname cannot be empty",
		},
		{
			name: "HostnameTooLong ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: strings.Repeat("a", 256),
				Port:     9735,
			},
			writer:    &bytes.Buffer{},
			expected:  nil,
			expectErr: true,
			errMsg: "hostname length is 256, exceeds maximum " +
				"length of 255 characters",
		},
		{
			name: "WriterError AddressTypeWrite ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &errorWriter{
				err: errors.New("address type write error"),
			},
			expected:  nil,
			expectErr: true,
			errMsg:    "address type write error",
		},
		// Error after writing address type.
		{
			name: "WriterError HostnameLengthWrite ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &countedErrorWriter{
				errAfter: 1,
				err: errors.New("hostname length write " +
					"error"),
			},
			expected:  nil,
			expectErr: true,
			errMsg:    "hostname length write error",
		},
		// Error after writing address type and hostname length.
		{
			name: "WriterError HostnameWrite ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &countedErrorWriter{
				errAfter: 2,
				err:      errors.New("hostname write error"),
			},
			expected:  nil,
			expectErr: true,
			errMsg:    "hostname write error",
		},
		// Error after writing address type, hostname length,
		// and hostname.
		{
			name: "WriterError PortWrite ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &countedErrorWriter{
				errAfter: 3,
				err:      errors.New("port write error"),
			},
			expected:  nil,
			expectErr: true,
			errMsg:    "port write error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := encodeDNSHostnameAddr(test.writer, test.addr)
			if test.expectErr {
				require.ErrorContains(t, err, test.errMsg)
				return
			}
			require.NoError(t, err)

			buffer, ok := test.writer.(*bytes.Buffer)
			require.True(t, ok)
			require.Equal(t, test.expected, buffer.Bytes())
		})
	}
}

// TestDecodeDNSHostnameAddress ensures correct decoding of DNSHostnameAddress
// from its binary representation. It covers valid cases, incomplete or
// malformed data, unknown types, and excessive input.
func TestDecodeDNSHostnameAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		data      []byte
		expected  *lnwire.DNSHostnameAddress
		expectErr bool
		errMsg    string
	}{
		{
			name: "ValidHostname ShortLength DecodesCorrectly",
			data: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x26, 0x07, // port 9735 in big-endian
			},
			expected: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			expectErr: false,
		},
		{
			name: "ValidHostname NonStandardPort DecodesCorrectly",
			data: []byte{
				byte(dnsHostnameAddr),
				17,
				'l', 'i', 'g', 'h', 't', 'n', 'i', 'n', 'g',
				'.', 'n', 'e', 't', 'w', 'o', 'r', 'k',
				0x04, 0xD2, // port 1234 in big-endian
			},
			expected: &lnwire.DNSHostnameAddress{
				Hostname: "lightning.network",
				Port:     1234,
			},
			expectErr: false,
		},
		{
			name:      "EmptyData ReturnsError",
			data:      []byte{},
			expected:  nil,
			expectErr: true,
			errMsg:    "EOF",
		},
		{
			name:      "IncompleteData MissingLength ReturnsError",
			data:      []byte{byte(dnsHostnameAddr)},
			expected:  nil,
			expectErr: true,
			errMsg:    "EOF",
		},
		{
			name: "IncompleteData MissingHostname ReturnsError",
			data: []byte{
				byte(dnsHostnameAddr),
				11, // Length of hostname that isn't there
			},
			expected:  nil,
			expectErr: true,
			errMsg:    "EOF",
		},
		{
			name: "IncompleteData MissingPort ReturnsError",
			data: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				// Missing port
			},
			expected:  nil,
			expectErr: true,
			errMsg:    "EOF",
		},
		{
			name: "IncompleteData PartialHostname ReturnsError",
			data: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c',
				0x26, 0x07,
			},
			expected:  nil,
			expectErr: true,
			errMsg:    "EOF",
		},
		{
			name: "IncompleteData PartialPort ReturnsError",
			data: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x2, // Only 1 byte of port
			},
			expected:  nil,
			expectErr: true,
			errMsg:    "expected to read 2 bytes for port",
		},
		{
			name: "ExcessiveData DecodesCorrectlyAndIgnoresExcess",
			data: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x26, 0x07,
				// Extra data
				0x01, 0x02, 0x03,
			},
			expected: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			expectErr: false,
		},
		{
			name: "UnknownAddressType ReturnsError",
			data: []byte{
				byte(0xFF),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x26, 0x07, // port 9735 in big-endian
			},
			expected: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			expectErr: true,
			errMsg:    ErrUnknownAddressType.Error(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := bytes.NewReader(test.data)
			addr, err := DeserializeAddr(r)
			if test.expectErr {
				require.ErrorContains(t, err, test.errMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, addr)
		})
	}
}

// errorWriter is a writer that always returns an error.
type errorWriter struct {
	err error
}

func (w *errorWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

// countedErrorWriter is a writer that returns an error after a specific
// number of writes.
type countedErrorWriter struct {
	writeCount int
	errAfter   int
	err        error
}

// Write implements the io.Writer interface for countedErrorWriter.
// It writes successfully for the first errAfter calls, then returns the
// configured error. This is used for testing error propagation after
// a specific number of writes.
func (w *countedErrorWriter) Write(p []byte) (int, error) {
	if w.writeCount >= w.errAfter {
		return 0, w.err
	}
	w.writeCount++

	return len(p), nil
}
