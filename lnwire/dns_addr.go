package lnwire

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// ErrEmptyDNSHostname is returned when a DNS hostname is empty.
var ErrEmptyDNSHostname = errors.New("hostname cannot be empty")

// ErrZeroPort is returned when a DNS port is zero.
var ErrZeroPort = errors.New("port cannot be zero")

// DNSAddress is used to represent a DNS address of a node.
type DNSAddress struct {
	// Hostname is the DNS hostname of the address. This MUST only contain
	// ASCII characters as per Bolt #7. The maximum length that this may
	// be is 255 bytes.
	Hostname string

	// Port is the port number of the address.
	Port uint16
}

// A compile-time check to ensure that DNSAddress implements the net.Addr
// interface.
var _ net.Addr = (*DNSAddress)(nil)

// Network returns the network that this address uses, which is "tcp".
func (d *DNSAddress) Network() string {
	return "tcp"
}

// String returns the address in the form "hostname:port".
func (d *DNSAddress) String() string {
	return net.JoinHostPort(d.Hostname, strconv.Itoa(int(d.Port)))
}

// ValidateDNSAddr validates that the DNS hostname is not empty and contains
// only ASCII characters and of max length 255 characters and port is non zero
// according to BOLT #7.
func ValidateDNSAddr(hostname string, port uint16) error {
	if hostname == "" {
		return ErrEmptyDNSHostname
	}

	// Per BOLT 7, ports must not be zero for type 5 address (DNS address).
	if port == 0 {
		return ErrZeroPort
	}

	if len(hostname) > 255 {
		return fmt.Errorf("DNS hostname length %d, exceeds limit of "+
			"255 bytes", len(hostname))
	}

	// Check if hostname contains only ASCII characters.
	for i, r := range hostname {
		// Check for valid hostname characters, excluding ASCII control
		// characters (0-31), spaces, underscores, delete character
		// (127), and the special characters (like /, \, @, #, $, etc.).
		if !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' ||
			r == '.') {

			return fmt.Errorf("hostname '%s' contains invalid "+
				"character '%c' at position %d", hostname, r, i)
		}
	}

	return nil
}
