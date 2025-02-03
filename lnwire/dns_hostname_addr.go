package lnwire

import (
	"fmt"
	"net"
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
