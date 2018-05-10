package torsvc

import (
	"net"
	"strconv"
)

// OnionAddress is a struct housing a hidden service (v2 & v3) as well as the
// Virtual Port that this hidden service can be reached at.
type OnionAddress struct {
	HiddenService string
	Port          int
}

// A compile-time check to ensure that OnionAddress implements the net.Addr
// interface.
var _ net.Addr = (*OnionAddress)(nil)

// String returns a string version of OnionAddress
func (o *OnionAddress) String() string {
	return net.JoinHostPort(o.HiddenService, strconv.Itoa(o.Port))
}

// Network returns the network that this implementation of net.Addr will use.
// In this case, because Tor only allows "tcp", the network is "tcp".
func (o *OnionAddress) Network() string {
	// Tor only allows "tcp"
	return "tcp"
}
