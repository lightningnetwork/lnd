package lnwire

import (
	"net"
	"strconv"
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

// NewDNSAddr creates a new DNS address with the given host and port.
func NewDNSAddr(host string, port int) (*DNSAddr, error) {
	dnsAddr := &DNSAddr{
		Hostname: host,
		Port:     port,
	}

	return dnsAddr, nil
}
