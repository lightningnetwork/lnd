package graphdb

import (
	"bytes"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
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

func TestEncodeDNSHostnameAddress(t *testing.T) {
	tests := []struct {
		name     string
		addr     *lnwire.DNSHostnameAddress
		writer   io.Writer
		expected []byte
		wantErr  bool
		errMsg   string
	}{
		{
			name: "ValidHostname_ShortLength_EncodesCorrectly",
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
			wantErr: false,
		},
		{
			name: "ValidHostname_MaximumLength_EncodesCorrectly",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: strings.Repeat("a", 255),
				Port:     8080,
			},
			writer: &bytes.Buffer{},
			expected: append(
				append(
					[]byte{byte(dnsHostnameAddr), 255},
					[]byte(strings.Repeat("a", 255))...,
				),
				[]byte{0x1F, 0x90}...,
			),
			wantErr: false,
		},
		{
			name: "ValidHostname_NonStandardPort_EncodesCorrectly",
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
			wantErr: false,
		},
		{
			name: "EmptyHostname_ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "",
				Port:     9735,
			},
			writer:   &bytes.Buffer{},
			expected: nil,
			wantErr:  true,
			errMsg:   "hostname cannot be empty",
		},
		{
			name: "HostnameTooLong_ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: strings.Repeat("a", 256),
				Port:     9735,
			},
			writer:   &bytes.Buffer{},
			expected: nil,
			wantErr:  true,
			errMsg: "hostname length is 256, exceeds maximum " +
				"length of 255 characters",
		},
		{
			name: "WriterError_AddressTypeWrite_ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &errorWriter{
				err: errors.New("address type write error"),
			},
			expected: nil,
			wantErr:  true,
			errMsg:   "address type write error",
		},
		// Error after writing address type.
		{
			name: "WriterError_HostnameLengthWrite_ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &countedErrorWriter{
				errAfter: 1,
				err: errors.New("hostname length write " +
					"error"),
			},
			expected: nil,
			wantErr:  true,
			errMsg:   "hostname length write error",
		},
		// Error after writing address type and hostname length.
		{
			name: "WriterError_HostnameWrite_ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &countedErrorWriter{
				errAfter: 2,
				err:      errors.New("hostname write error"),
			},
			expected: nil,
			wantErr:  true,
			errMsg:   "hostname write error",
		},
		// Error after writing address type, hostname length,
		// and hostname.
		{
			name: "WriterError_PortWrite_ReturnsError",
			addr: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     9735,
			},
			writer: &countedErrorWriter{
				errAfter: 3,
				err:      errors.New("port write error"),
			},
			expected: nil,
			wantErr:  true,
			errMsg:   "port write error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := encodeDNSHostnameAddr(test.writer, test.addr)

			if err == nil && test.wantErr {
				t.Fatal("expected an error, but got none")
			}
			if err != nil && !test.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && test.wantErr &&
				!strings.Contains(err.Error(), test.errMsg) {

				t.Errorf("expected error message %q to be "+
					"in %q", test.errMsg, err.Error())
			}

			if !test.wantErr {
				buffer, ok := test.writer.(*bytes.Buffer)
				if !ok {
					t.Fatal("test.writer is not " +
						"a *bytes.Buffer")
				}
				result := buffer.Bytes()
				if !bytes.Equal(result, test.expected) {
					t.Errorf("encodeDNSHostnameAddr() = "+
						"%v, want %v", result,
						test.expected)
				}
			}
		})
	}
}

func TestDecodeDNSHostnameAddress(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected *lnwire.DNSHostnameAddress
		wantErr  bool
		errMsg   string
	}{
		{
			name: "ValidHostname_ShortLength_DecodesCorrectly",
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
			wantErr: false,
		},
		{
			name: "ValidHostname_NonStandardPort_DecodesCorrectly",
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
			wantErr: false,
		},
		{
			name:     "EmptyData_ReturnsError",
			data:     []byte{},
			expected: nil,
			wantErr:  true,
			errMsg:   "EOF",
		},
		{
			name:     "IncompleteData_MissingLength_ReturnsError",
			data:     []byte{byte(dnsHostnameAddr)},
			expected: nil,
			wantErr:  true,
			errMsg:   "EOF",
		},
		{
			name: "IncompleteData_MissingHostname_ReturnsError",
			data: []byte{
				byte(dnsHostnameAddr),
				11, // Length of hostname that isn't there
			},
			expected: nil,
			wantErr:  true,
			errMsg:   "EOF",
		},
		{
			name: "IncompleteData_MissingPort_ReturnsError",
			data: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				// Missing port
			},
			expected: nil,
			wantErr:  true,
			errMsg:   "EOF",
		},
		{
			name: "IncompleteData_PartialHostname_ReturnsError",
			data: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c',
				0x26, 0x07,
			},
			expected: nil,
			wantErr:  true,
			errMsg:   "EOF",
		},
		{
			name: "IncompleteData_PartialPort_ReturnsError",
			data: []byte{
				byte(dnsHostnameAddr),
				11,
				'e', 'x', 'a', 'm', 'p', 'l', 'e',
				'.', 'c', 'o', 'm',
				0x2, // Only 1 byte of port
			},
			expected: nil,
			wantErr:  true,
			errMsg:   "expected to read 2 bytes for port",
		},
		{
			name: "ExcessiveData_DecodesCorrectlyAndIgnoresExcess",
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
			wantErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := bytes.NewReader(test.data)

			addr, err := DeserializeAddr(r)

			if err == nil && test.wantErr {
				t.Fatal("expected an error, but got none")
			}
			if err != nil && !test.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && test.wantErr &&
				!strings.Contains(err.Error(), test.errMsg) {

				t.Errorf("expected error message %q to be "+
					"in %q", test.errMsg, err.Error())
			}

			if !test.wantErr {
				if !reflect.DeepEqual(addr, test.expected) {
					t.Errorf("decodeDNSHostnameAddr() = "+
						"%+v, want %+v", addr,
						test.expected)
				}
			}
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

func (w *countedErrorWriter) Write(p []byte) (int, error) {
	if w.writeCount >= w.errAfter {
		return 0, w.err
	}
	w.writeCount++

	return len(p), nil
}
