package lnwire

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestValidateDNSAddr tests hostname and port validation per BOLT #7.
func TestValidateDNSAddr(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		hostname string
		port     uint16
		err      error
	}{
		{
			name:     "empty hostname",
			hostname: "",
			port:     9735,
			err:      ErrEmptyDNSHostname,
		},
		{
			name:     "zero port",
			hostname: "example.com",
			port:     0,
			err:      ErrZeroPort,
		},
		{
			name:     "hostname too long",
			hostname: strings.Repeat("a", 256),
			port:     9735,
			err: fmt.Errorf("%w: DNS hostname length 256",
				ErrHostnameTooLong),
		},
		{
			name:     "hostname with invalid ASCII space",
			hostname: "exa mple.com",
			port:     9735,
			err: fmt.Errorf("%w: hostname 'exa mple.com' contains "+
				"invalid character ' ' at position 3",
				ErrInvalidHostnameCharacter),
		},
		{
			name:     "hostname with invalid ASCII underscore",
			hostname: "example_node.com",
			port:     9735,
			err: fmt.Errorf("%w: hostname 'example_node.com' "+
				"contains invalid character '_' at position 7",
				ErrInvalidHostnameCharacter),
		},
		{
			name:     "hostname with non-ASCII character",
			hostname: "example❄️.com",
			port:     9735,
			err: fmt.Errorf("%w: hostname 'example❄️.com' "+
				"contains invalid character '❄' at position 7",
				ErrInvalidHostnameCharacter),
		},
		{
			name:     "valid hostname",
			hostname: "example.com",
			port:     9735,
		},
		{
			name:     "valid hostname with numbers",
			hostname: "node101.example.com",
			port:     9735,
		},
		{
			name:     "valid hostname with hyphens",
			hostname: "my-node.example-domain.com",
			port:     9735,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDNSAddr(tc.hostname, tc.port)
			require.Equal(t, err, tc.err)
		})
	}
}

// TestDNSAddressTLVEncoding tests the TLV encoding and decoding of DNSAddress
// structs using the ExtraOpaqueData interface.
func TestDNSAddressTLVEncoding(t *testing.T) {
	t.Parallel()

	testDNSAddr := DNSAddress{
		Hostname: "lightning.example.com",
		Port:     9000,
	}

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&testDNSAddr))

	var decodedDNSAddr DNSAddress
	tlvs, err := extraData.ExtractRecords(&decodedDNSAddr)
	require.NoError(t, err)

	require.Contains(t, tlvs, tlv.Type(0))
	require.Equal(t, testDNSAddr, decodedDNSAddr)
}

// TestDNSAddressRecord tests the TLV Record interface of DNSAddress
// by directly encoding and decoding using the Record method.
func TestDNSAddressRecord(t *testing.T) {
	t.Parallel()

	testDNSAddr := DNSAddress{
		Hostname: "lightning.example.com",
		Port:     9000,
	}

	var buf bytes.Buffer
	record := testDNSAddr.Record()
	require.NoError(t, record.Encode(&buf))

	var decodedDNSAddr DNSAddress
	decodedRecord := decodedDNSAddr.Record()
	require.NoError(t, decodedRecord.Decode(&buf, uint64(buf.Len())))

	require.Equal(t, testDNSAddr, decodedDNSAddr)
}

// TestDNSAddressInvalidDecoding tests error cases during TLV decoding.
func TestDNSAddressInvalidDecoding(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		data   []byte
		errMsg string
	}{
		{
			name:   "too short (only 1 byte)",
			data:   []byte{0x61},
			errMsg: "DNS address must be at least 2 bytes",
		},
		{
			name:   "empty data",
			data:   []byte{},
			errMsg: "DNS address must be at least 2 bytes",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var dnsAddr DNSAddress
			record := dnsAddr.Record()

			buf := bytes.NewReader(tc.data)
			err := record.Decode(buf, uint64(len(tc.data)))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.errMsg)
		})
	}
}

// TestDNSAddressProperty uses property-based testing to verify that DNSAddress
// TLV encoding and decoding is correct for random DNSAddress values.
func TestDNSAddressProperty(t *testing.T) {
	t.Parallel()

	scenario := func(t *rapid.T) {
		// Generate a random valid hostname.
		hostname := genValidHostname(t)

		// Generate a random port (excluding 0 which is invalid).
		port := rapid.Uint16Range(1, 65535).Draw(t, "port")

		dnsAddr := DNSAddress{
			Hostname: hostname,
			Port:     port,
		}

		var buf bytes.Buffer
		record := dnsAddr.Record()
		err := record.Encode(&buf)
		require.NoError(t, err)

		var decodedDNSAddr DNSAddress
		decodedRecord := decodedDNSAddr.Record()
		err = decodedRecord.Decode(&buf, uint64(buf.Len()))
		require.NoError(t, err)

		require.Equal(t, dnsAddr, decodedDNSAddr)
	}

	rapid.Check(t, scenario)
}
