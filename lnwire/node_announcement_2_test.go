package lnwire

import (
	"bytes"
	"math"
	"net"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// TestNodeAnn2AddressPortRange verifies that locally constructed IP and Tor
// addresses cannot wrap an out-of-range port into a different wire value.
func TestNodeAnn2AddressPortRange(t *testing.T) {
	t.Parallel()

	onion := tor.Base32Encoding.EncodeToString(make([]byte, 35)) +
		tor.OnionSuffix
	tests := []struct {
		name   string
		encode func(int) error
	}{
		{
			name: "ipv4",
			encode: func(port int) error {
				addrs := IPV4Addrs{{
					IP: net.IPv4(192, 0, 2, 1), Port: port,
				}}

				return ipv4AddrsEncoder(
					&bytes.Buffer{}, &addrs, nil,
				)
			},
		},
		{
			name: "ipv6",
			encode: func(port int) error {
				addrs := IPV6Addrs{{
					IP:   net.ParseIP("2001:db8::1"),
					Port: port,
				}}

				return ipv6AddrsEncoder(
					&bytes.Buffer{}, &addrs, nil,
				)
			},
		},
		{
			name: "tor v3",
			encode: func(port int) error {
				addrs := TorV3Addrs{{
					OnionService: onion, Port: port,
				}}

				return torV3AddrsEncoder(
					&bytes.Buffer{}, &addrs, nil,
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			for _, port := range []int{0, math.MaxUint16} {
				require.NoError(t, test.encode(port))
			}

			require.ErrorIs(
				t, test.encode(-1), ErrNodeAnn2PortOutOfRange,
			)
			require.ErrorIs(
				t, test.encode(math.MaxUint16+1),
				ErrNodeAnn2PortOutOfRange,
			)
		})
	}
}

// TestNodeAnn2EncodeDecode tests the encoding and decoding of the
// NodeAnnouncement2 message using hardcoded byte slices.
func TestNodeAnn2EncodeDecode(t *testing.T) {
	t.Parallel()

	// We'll create a raw byte stream that represents a valid
	// NodeAnnouncement2 message with various known and unknown fields in
	// the signed TLV ranges along with the signature in the unsigned range.
	rawBytes := []byte{
		// Features record.
		0x00,     // type.
		0x02,     // length.
		0x1, 0x2, // value.

		// Color record (optional).
		0x01,             // type.
		0x03,             // length.
		0xff, 0x00, 0x80, // value: RGB(255, 0, 128).

		// BlockHeight record.
		0x02,                   // type.
		0x04,                   // length.
		0x00, 0x01, 0x86, 0xa0, // value: 100000.

		// Alias record (optional).
		0x03, // type.
		0x20, // length (32).
		0x4c, 0x69, 0x67, 0x68, 0x74, 0x6e, 0x69, 0x6e,
		0x67, 0x20, 0x4e, 0x6f, 0x64, 0x65, 0x20, 0x54,
		0x65, 0x73, 0x74, 0x20, 0x41, 0x6c, 0x69, 0x61,
		0x73, 0x20, 0x4e, 0x61, 0x6d, 0x65, 0x21, 0x00, // value.

		// NodeID record.
		0x04, // type.
		0x21, // length.
		0x02, 0x28, 0xf2, 0xaf, 0xa, 0xbe, 0x32, 0x24, 0x3, 0x48, 0xf,
		0xb3, 0xee, 0x17, 0x2f, 0x7f, 0x16, 0x1, 0xe6, 0x7d, 0x1d, 0xa6,
		0xca, 0xd4, 0xb, 0x54, 0xc4, 0x46, 0x8d, 0x48, 0x23, 0x6c, 0x39,

		// IPV4Addrs record (optional).
		0x05,                               // type.
		0x0c,                               // length (12 bytes).
		0x0a, 0x00, 0x00, 0x01, 0x23, 0x28, // 10.0.0.1:9000
		0x0a, 0x00, 0x00, 0x01, 0x1f, 0x90, // 10.0.0.1:8080

		// IPV6Addrs record (optional).
		0x07,                                           // type.
		0x12,                                           // length.
		0x20, 0x01, 0x0d, 0xb8, 0x00, 0x00, 0x00, 0x00, // IPv6 address
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, // 2001:db8::1
		0x1f, 0x40, // port 8000

		// TorV3Addrs record (optional).
		0x09, // type.
		0x25, // length (37 bytes = 1 address).
		// value: 35-byte tor v3 address + 2-byte port.
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
		0x21, 0x22, 0x23,
		0x23, 0x28, // port 9000.

		// DNSHostNames record (optional): one entry of u16 length,
		// hostname and u16 port.
		0x0b,       // type (11).
		0x19,       // length (25).
		0x00, 0x15, // hostname length (21).
		0x6c, 0x69, 0x67, 0x68, 0x74, 0x6e, 0x69, 0x6e,
		0x67, 0x2e, 0x65, 0x78, 0x61, 0x6d, 0x70, 0x6c, // value.
		0x65, 0x2e, 0x63, 0x6f, 0x6d,
		0x23, 0x28, // port 9000.

		// Signature.
		0xf0, // type.
		0x40, // length.
		0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8, 0x9, 0xa, 0xb,
		0xc, 0xd, 0xe, 0xf, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16,
		0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
		0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a,
		0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30, 0x31, 0x32, 0x33, 0x34,
		0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e,
		0x3f, // value.
	}
	secondSignedRangeType := new(bytes.Buffer)
	var buf [8]byte
	err := tlv.WriteVarInt(
		secondSignedRangeType, pureTLVSignedSecondRangeStart+1, &buf,
	)
	require.NoError(t, err)
	rawBytes = append(rawBytes, secondSignedRangeType.Bytes()...) // type.
	rawBytes = append(rawBytes, []byte{
		0x02,       // length.
		0x79, 0x79, // value.
	}...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &NodeAnnouncement2{}
	r := bytes.NewReader(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	// Next, encode the message back into a new byte buffer.
	var b bytes.Buffer
	err = msg.Encode(&b, 0)
	require.NoError(t, err)

	// The re-encoded bytes should be exactly the same as the original raw
	// bytes.
	require.Equal(t, rawBytes, b.Bytes())
}

// TestNodeAnn2AddressesIgnorePortZero tests that an address with a zero port,
// or a DNS entry with an invalid hostname, drops only that address. The
// message must still round-trip byte for byte, because the signature digest
// is rebuilt from the decoded records.
func TestNodeAnn2AddressesIgnorePortZero(t *testing.T) {
	t.Parallel()

	onion := tor.Base32Encoding.EncodeToString(make([]byte, 35)) +
		tor.OnionSuffix

	var (
		ip4 = &net.TCPAddr{
			IP: net.IPv4(10, 0, 0, 1).To4(), Port: 9735,
		}
		ip4Zero = &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1).To4()}
		ip6     = &net.TCPAddr{
			IP: net.ParseIP("2001:db8::1"), Port: 9735,
		}
		ip6Zero = &net.TCPAddr{IP: net.ParseIP("2001:db8::2")}
		onn     = &tor.OnionAddr{OnionService: onion, Port: 9735}
		dns     = &DNSAddress{Hostname: "example.com", Port: 9735}
	)

	// Put a usable address before a distinct zero-port address. This
	// ensures that decoding the latter cannot overwrite the former's IP
	// bytes.
	msg := &NodeAnnouncement2{}
	msg.Signature.Val = testSchnorrSig
	msg.IPV4Addrs = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType5](
		IPV4Addrs{
			ip4,
			ip4Zero,
		},
	))
	msg.IPV6Addrs = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType7](
		IPV6Addrs{
			ip6,
			ip6Zero,
		},
	))
	msg.TorV3Addrs = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType9](
		TorV3Addrs{
			onn,
			{OnionService: onion, Port: 0},
		},
	))
	msg.DNSHostNames = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType11](
		DNSAddrs{
			{Hostname: "example.org", Port: 0},
			{Hostname: "bad_host", Port: 9735},
			dns,
		},
	))

	// The codec keeps every entry, including the unusable ones.
	var b bytes.Buffer
	require.NoError(t, msg.Encode(&b, 0))

	var decoded NodeAnnouncement2
	require.NoError(t, decoded.Decode(bytes.NewReader(b.Bytes()), 0))

	var reencoded bytes.Buffer
	require.NoError(t, decoded.Encode(&reencoded, 0))
	require.Equal(t, b.Bytes(), reencoded.Bytes())

	// Only the usable addresses remain, in field order.
	require.Equal(t, []net.Addr{ip4, ip6, onn, dns}, decoded.Addresses())
}
