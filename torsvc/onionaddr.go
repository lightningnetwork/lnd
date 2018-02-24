package torsvc

import (
	"net"
	"strconv"
)

type OnionAddress struct {
	HiddenService string
	Port int
}

// A compile-time check to ensure that OnionAddress implements the net.Addr
// interface.
var _ net.Addr = (*OnionAddress)(nil)

func (o *OnionAddress) String() string {
	return net.JoinHostPort(o.HiddenService, strconv.Itoa(o.Port))
}

func (o *OnionAddress) Network() string {
	// Tor only allows "tcp"
	return "tcp"
}
