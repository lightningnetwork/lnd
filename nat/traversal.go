package nat

import (
	"errors"
	"net"
)

var (
	// private24BitBlock contains the set of private IPv4 addresses within
	// the 10.0.0.0/8 address space.
	private24BitBlock *net.IPNet

	// private20BitBlock contains the set of private IPv4 addresses within
	// the 172.16.0.0/12 address space.
	private20BitBlock *net.IPNet

	// private16BitBlock contains the set of private IPv4 addresses within
	// the 192.168.0.0/16 address space.
	private16BitBlock *net.IPNet

	// ErrMultipleNAT is an error returned when multiple NATs have been
	// detected.
	ErrMultipleNAT = errors.New("multiple NATs detected")
)

func init() {
	_, private24BitBlock, _ = net.ParseCIDR("10.0.0.0/8")
	_, private20BitBlock, _ = net.ParseCIDR("172.16.0.0/12")
	_, private16BitBlock, _ = net.ParseCIDR("192.168.0.0/16")
}

// Traversal is an interface that brings together the different NAT traversal
// techniques.
type Traversal interface {
	// ExternalIP returns the external IP address.
	ExternalIP() (net.IP, error)

	// AddPortMapping adds a port mapping for the given port between the
	// private and public addresses.
	AddPortMapping(port uint16) error

	// DeletePortMapping deletes a port mapping for the given port between
	// the private and public addresses.
	DeletePortMapping(port uint16) error

	// ForwardedPorts returns the ports currently being forwarded using NAT
	// traversal.
	ForwardedPorts() []uint16

	// Name returns the name of the specific NAT traversal technique used.
	Name() string
}

// isPrivateIP determines if the IP is private.
func isPrivateIP(ip net.IP) bool {
	return private24BitBlock.Contains(ip) ||
		private20BitBlock.Contains(ip) || private16BitBlock.Contains(ip)
}
