package lnwire

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// hexOf returns the hex encoding of a hostname, for building wire vectors.
func hexOf(hostname string) string {
	return hex.EncodeToString([]byte(hostname))
}

// TestDNSAddrsWireFormat tests that the dns_hostnames list uses the spec
// layout of a u16 hostname length, the hostname and a u16 port per entry, and
// that the codec round-trips entries it does not validate, such as a zero
// port.
func TestDNSAddrsWireFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		expected  DNSAddrs
		expectErr bool
	}{
		{
			name: "single entry",
			raw:  "000b" + hexOf("example.com") + "2607",
			expected: DNSAddrs{
				{Hostname: "example.com", Port: 9735},
			},
		},
		{
			name: "two entries",
			raw: "0003" + hexOf("a.b") + "0001" +
				"0003" + hexOf("c.d") + "ffff",
			expected: DNSAddrs{
				{Hostname: "a.b", Port: 1},
				{Hostname: "c.d", Port: 65535},
			},
		},
		{
			// The codec must not reject a zero port. The entry is
			// signed, so dropping or refusing it here would break
			// signature verification.
			name: "zero port round-trips",
			raw:  "0003" + hexOf("a.b") + "0000",
			expected: DNSAddrs{
				{Hostname: "a.b", Port: 0},
			},
		},
		{
			name:      "entry shorter than its overhead",
			raw:       "000b00",
			expectErr: true,
		},
		{
			name:      "hostname length overruns the value",
			raw:       "000b" + hexOf("a.b") + "0001",
			expectErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			raw, err := hex.DecodeString(test.raw)
			require.NoError(t, err)

			var addrs DNSAddrs
			rec := addrs.Record()
			err = rec.Decode(
				bytes.NewReader(raw), uint64(len(raw)),
			)
			if test.expectErr {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, addrs)

			// Re-encoding must reproduce the exact bytes, and the
			// advertised size must match what the encoder writes.
			var b bytes.Buffer
			require.NoError(t, rec.Encode(&b))
			require.Equal(t, raw, b.Bytes())
			require.EqualValues(t, len(raw), rec.Size())
		})
	}
}
