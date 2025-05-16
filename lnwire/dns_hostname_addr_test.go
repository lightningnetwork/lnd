package lnwire

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDNSHostnameAddressValidate tests the validation logic for
// DNSHostnameAddress, verifying hostname and port validation rules through test
// cases that check for empty hostnames, length limits, port number ranges,
// non-ASCII characters, control characters, and various valid hostname formats
// and port combinations.
func TestDNSHostnameAddressValidate(t *testing.T) {
	tests := []struct {
		name      string
		hostname  string
		port      int
		expectErr bool
		errMsg    string
	}{
		{
			name:      "empty hostname",
			hostname:  "",
			port:      80,
			expectErr: true,
			errMsg:    "hostname cannot be empty",
		},
		{
			name:      "hostname too long (256 chars)",
			hostname:  strings.Repeat("a", 256),
			port:      80,
			expectErr: true,
			errMsg:    "exceeds maximum length of 255 characters",
		},
		{
			name:      "empty label",
			hostname:  "example..com",
			port:      80,
			expectErr: true,
			errMsg:    "hostname contains an empty label",
		},
		{
			name:      "label too long (64 chars)",
			hostname:  strings.Repeat("a", 64) + ".com",
			port:      80,
			expectErr: true,
			errMsg:    "exceeds maximum length of 63 characters",
		},
		{
			name:      "label starts with hyphen",
			hostname:  "-example.com",
			port:      80,
			expectErr: true,
			errMsg:    "starts or ends with a hyphen",
		},
		{
			name:      "label ends with hyphen",
			hostname:  "example-.com",
			port:      80,
			expectErr: true,
			errMsg:    "starts or ends with a hyphen",
		},
		{
			name:      "port zero",
			hostname:  "example.com",
			port:      0,
			expectErr: true,
			errMsg:    "invalid port number 0",
		},
		{
			name:      "negative port",
			hostname:  "example.com",
			port:      -1,
			expectErr: true,
			errMsg:    "invalid port number -1",
		},
		{
			name:      "port too high",
			hostname:  "example.com",
			port:      65536,
			expectErr: true,
			errMsg:    "invalid port number 65536",
		},
		{
			name:      "hostname with non-ASCII character",
			hostname:  "café.com",
			port:      80,
			expectErr: true,
			errMsg:    "contains an invalid character 'é'",
		},
		{
			name:      "hostname with another non-ASCII character",
			hostname:  "example.com\u2022", // bullet character
			port:      80,
			expectErr: true,
			errMsg:    "contains an invalid character '•'",
		},
		{
			name:      "hostname with multiple non-ASCII chars",
			hostname:  "世界.com",
			port:      80,
			expectErr: true,
			errMsg:    "contains an invalid character '世'",
		},
		{
			name:      "hostname with control character",
			hostname:  "example\n.com",
			port:      80,
			expectErr: true,
			errMsg:    "contains an invalid character '\n'",
		},
		{
			name:     "valid hostname with standard port",
			hostname: "example.com",
			port:     80,
		},
		{
			name:     "valid hostname with high port",
			hostname: "lightning.network",
			port:     65535,
		},
		{
			name:     "valid hostname with low port",
			hostname: "test.local",
			port:     1,
		},
		{
			name:     "valid subdomain hostname",
			hostname: "api.service.example.org",
			port:     8080,
		},
		{
			name:     "valid numeric hostname",
			hostname: "123.example.com",
			port:     9735,
		},
		{
			name:     "valid IP-like hostname",
			hostname: "127.0.0.1",
			port:     9735,
		},
		{
			name:     "valid hostname with dash",
			hostname: "test-server.example.com",
			port:     443,
		},
		{
			name:     "valid hostname with label length 63 chars",
			hostname: strings.Repeat("a", 63) + ".com",
			port:     8888,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addr := &DNSHostnameAddress{
				Hostname: test.hostname,
				Port:     test.port,
			}

			err := addr.Validate()
			if test.expectErr {
				require.ErrorContains(t, err, test.errMsg)
				return
			}
			require.NoError(t, err)
		})
	}
}
