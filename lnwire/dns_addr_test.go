package lnwire

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateDNSAddr tests hostname and port validation per BOLT #7.
func TestValidateDNSAddr(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		hostname     string
		port         uint16
		expectErr    bool
		expectErrMsg string
	}{
		{
			name:         "empty hostname",
			hostname:     "",
			port:         9735,
			expectErr:    true,
			expectErrMsg: ErrEmptyDNSHostname.Error(),
		},
		{
			name:         "zero port",
			hostname:     "example.com",
			port:         0,
			expectErr:    true,
			expectErrMsg: ErrZeroPort.Error(),
		},
		{
			name:      "hostname too long",
			hostname:  strings.Repeat("a", 256),
			port:      9735,
			expectErr: true,
			expectErrMsg: "DNS hostname length 256, exceeds " +
				"limit of 255 bytes",
		},
		{
			name:      "hostname with invalid ASCII space",
			hostname:  "exa mple.com",
			port:      9735,
			expectErr: true,
			expectErrMsg: "hostname 'exa mple.com' contains " +
				"invalid character ' ' at position 3",
		},
		{
			name:      "hostname with invalid ASCII underscore",
			hostname:  "example_node.com",
			port:      9735,
			expectErr: true,
			expectErrMsg: "hostname 'example_node.com' contains " +
				"invalid character '_' at position 7",
		},
		{
			name:      "hostname with non-ASCII character",
			hostname:  "example❄️.com",
			port:      9735,
			expectErr: true,
			expectErrMsg: "hostname 'example❄️.com' contains " +
				"invalid character '❄' at position 7",
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
			if tc.expectErr {
				require.ErrorContains(t, err, tc.expectErrMsg)
				return
			}
			require.NoError(t, err)
		})
	}
}
