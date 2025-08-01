package lnwire

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// ErrMultipleDNSAddresses indicates a violation of Bolt-07 protocol constraint
// that restricts to a single DNS address as yet.
var ErrMultipleDNSAddresses = errors.New("cannot advertise multiple DNS " +
	"addresses. See BOLT 7")

// ErrEmptyDNSHostname is returned when a DNS hostname is empty.
var ErrEmptyDNSHostname = errors.New("hostname cannot be empty")

// ErrZeroPort is returned when a DNS port is zero.
var ErrZeroPort = errors.New("port cannot be zero")

// DNSAddr is a custom implementation of the net.Addr interface.
type DNSAddr struct {
	// Hostname represents the DNS hostname (e.g., "example.com").
	Hostname string

	// Port is the network port number (e.g., 9735 for LND peer port).
	Port uint16
}

// NewDNSAddr creates a new DNS address with the given host and port. It also
// ensures the created DNS one is a valid one.
func NewDNSAddr(host string, port uint16) (*DNSAddr, error) {
	dnsAddr := &DNSAddr{
		Hostname: host,
		Port:     port,
	}

	if err := dnsAddr.Validate(); err != nil {
		return nil, err
	}

	return dnsAddr, nil
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
	return net.JoinHostPort(d.Hostname, strconv.Itoa(int(d.Port)))
}

// Validate validates that the DNS hostname is not empty and contains only ASCII
// characters and of max length 255 characters according to BOLT specifications.
// It also validates that the port is non-zero.
func (d *DNSAddr) Validate() error {
	// Check if hostname is empty.
	if d.Hostname == "" {
		return ErrEmptyDNSHostname
	}

	// Per BOLT 7, ports must not be zero for type 5 address (DNS address).
	if d.Port == 0 {
		return ErrZeroPort
	}

	// Ensure the hostname does not exceed the maximum allowed length of 255
	// octets.
	if len(d.Hostname) > 255 {
		return fmt.Errorf("hostname length is %d, exceeds maximum "+
			"length of 255 characters", len(d.Hostname))
	}

	// Check if hostname contains only ASCII characters.
	for i, r := range d.Hostname {
		// Check for valid hostname characters, excluding ASCII control
		// characters (0-31), spaces, underscores, delete character
		// (127), and the special characters (like /, \, @, #, $, etc.).
		if !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' ||
			r == '.') {

			return fmt.Errorf("hostname '%s' contains invalid "+
				"character '%c' at position %d", d.Hostname, r,
				i)
		}
		if r > 127 {
			return fmt.Errorf("hostname '%s' contains invalid "+
				"character '%c' at position %d", d.Hostname, r,
				i)
		}
	}

	return nil
}
