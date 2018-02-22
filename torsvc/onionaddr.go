package torsvc

import (
	"net"
	"fmt"
)

type OnionAddress struct {
	HiddenService []byte
}

// A compile-time check to ensure that OnionAddress implements the net.Addr
// interface.
var _ net.Addr = (*OnionAddress)(nil)

func (o *OnionAddress) String() string {
	return fmt.Sprintf("%s", o.HiddenService)
}

func (o *OnionAddress) Network() string {
	// Tor only allows "tcp"
	return "tcp"
}
