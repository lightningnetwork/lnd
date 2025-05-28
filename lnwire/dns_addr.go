package lnwire

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
)

// DNSAddr is a custom implementation of the net.Addr interface.
type DNSAddr struct {
	// Hostname represents the DNS hostname (e.g., "example.com").
	Hostname string

	// Port is the network port number (e.g., 9735 for LND peer port).
	Port int
}

// A compile-time check to ensure that DNSAddr implements the net.Addr
// interface.
var _ net.Addr = (*DNSAddr)(nil)

// Network returns the network type, e.g., "tcp".
func (d *DNSAddr) Network() string {
	return "tcp"
}

// String returns the address in the form "hostname:port".
func (d *DNSAddr) String() string {
	return net.JoinHostPort(d.Hostname, strconv.Itoa(d.Port))
}

// NewDNSAddr creates a new DNS address with the given host and port. It returns
// an error if the hostname is invalid according to DNS standards/BOLT 07
// specifications or if the port is outside the valid TCP
// range (1-65535).
func NewDNSAddr(host string, port int) (*DNSAddr, error) {
	dnsAddr := &DNSAddr{
		Hostname: host,
		Port:     port,
	}

	if err := dnsAddr.validate(); err != nil {
		return nil, err
	}

	return dnsAddr, nil
}

// validate checks that the DNSHostnameAddress is well-formed according to
// Lightning Network's BOLT 07 specification requirements. It ensures the
// hostname is not empty, doesn't exceed 255 characters, contains only ASCII
// characters, and that the port number is within the valid TCP range (1-65535).
func (d *DNSAddr) validate() error {
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

	// Check if the hostname resembles an IPv4 address.
	if IsIPv4Like(d.Hostname) {
		return fmt.Errorf("hostname '%s' resembles an IPv4 address "+
			"and cannot be used as a valid DNS hostname",
			d.Hostname)
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
			if !(unicode.IsLetter(ch) ||
				unicode.IsDigit(ch) || (ch == '-')) {

				return fmt.Errorf("hostname label '%s' "+
					"contains an invalid character '%c'",
					label, ch)
			}
		}
	}

	// Validate port number. Valid TCP ports are in the range 1-65535.
	if d.Port < 1 || d.Port > 65535 {
		return fmt.Errorf("invalid port number %d, must be between 1 "+
			"and 65535", d.Port)
	}

	return nil
}

// IsIPv4Like checks if the given address resembles an IPv4 address
// structure (e.g., "500.0.0.0"). It does not validate if the numeric
// values are within the standard IPv4 range (0-255).
func IsIPv4Like(address string) bool {
	// Split the address into parts using ".".
	parts := strings.Split(address, ".")
	if len(parts) != 4 {
		// IPv4-like addresses must have exactly 4 parts.
		return false
	}

	// Check each part to ensure it's numeric.
	for _, part := range parts {
		if _, err := strconv.Atoi(part); err != nil {
			// Part is not numeric, so it's not IPv4-like.
			return false
		}
	}

	return true
}
