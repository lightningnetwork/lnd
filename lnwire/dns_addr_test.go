package lnwire

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
