package lnwire

import (
	"net"
	"strconv"
)

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
