package lnwire

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// DNSHostnameAddress is a custom implementation of the net.Addr interface.
type DNSHostnameAddress struct {
	Hostname string
	Port     int
}

// A compile-time check to ensure that DNSHostnameAddr implements
// the net.Addr interface.
var _ net.Addr = (*DNSHostnameAddress)(nil)

// Network returns the network type, e.g., "tcp".
func (d *DNSHostnameAddress) Network() string {
	return "tcp"
}

// String returns the address in the form "hostname:port".
func (d *DNSHostnameAddress) String() string {
	return fmt.Sprintf("%s:%d", d.Hostname, d.Port)
}

// Validate checks that the DNSHostnameAddress is well-formed according to
// Lightning Network's BOLT 07 specification requirements. It ensures the
// hostname is not empty, doesn't exceed 255 characters, contains only ASCII
// characters, and that the port number is within the valid TCP range (1-65535).
func (d *DNSHostnameAddress) Validate() error {
	// Ensure the hostname is not empty.
	if len(d.Hostname) == 0 {
		return errors.New("hostname cannot be empty")
	}

	// Ensure the hostname does not exceed the maximum allowed length of 255
	// octets. This limit includes the length of all labels, separators
	// (dots), and the null terminator.
	if len(d.Hostname) > 255 {
		return fmt.Errorf("hostname length is %d, exceeds maximum "+
			"length of 255 characters", len(d.Hostname))
	}

	// Split hostname into labels and validate each.
	labels := strings.Split(d.Hostname, ".")
	for _, label := range labels {
		// RFC 1035, Section 2.3.4: Labels must not be empty.
		if len(label) == 0 {
			return errors.New("hostname contains an empty label")
		}

		// RFC 1035, Section 2.3.4: The maximum length of a label is 63
		// characters.
		if len(label) > 63 {
			return fmt.Errorf("hostname label '%s' exceeds "+
				"maximum length of 63 characters", label)
		}

		// RFC 952 and RFC 1123: Labels must not start or end with a
		// hyphen. RFC 1123 relaxed the rules from RFC 952 to allow
		// hostnames to start with a digit, but the rule about hyphens
		// remains.
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("hostname label '%s' starts or ends "+
				"with a hyphen", label)
		}

		// RFC 1035, Section 2.3.1: Labels must consist of only ASCII
		// letters (a-z, A-Z), digits (0-9), and hyphens (-). Any other
		// character, including non-ASCII characters, underscores, or
		// symbols, is invalid. BOLT 07 enforces this rule to ensure
		// node addresses are compatible with DNS standards.
		for _, ch := range label {
			if !((ch >= 'a' && ch <= 'z') ||
				(ch >= 'A' && ch <= 'Z') ||
				(ch >= '0' && ch <= '9') || (ch == '-')) {

				return fmt.Errorf("hostname label '%s' "+
					"contains an invalid character '%c'",
					label, ch)
			}
		}
	}

	// Validate port number. Valid TCP ports are in the range 1-65535.
	if d.Port <= 0 || d.Port > 65535 {
		return fmt.Errorf("invalid port number %d, must be between 1 "+
			"and 65535", d.Port)
	}

	return nil
}
