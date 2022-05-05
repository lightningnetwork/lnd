package lnwire

import (
	"encoding/hex"
	"net"
)

// OpaqueAddrs is used to store the address bytes for address types that are
// unknown to us.
type OpaqueAddrs struct {
	Payload []byte
}

// A compile-time assertion to ensure that OpaqueAddrs meets the net.Addr
// interface.
var _ net.Addr = (*OpaqueAddrs)(nil)

// String returns a human-readable string describing the target OpaqueAddrs.
// Since this is an unknown address (and could even be multiple addresses), we
// just return the hex string of the payload.
//
// This part of the net.Addr interface.
func (o *OpaqueAddrs) String() string {
	return hex.EncodeToString(o.Payload)
}

// Network returns the name of the network this address is bound to. Since this
// is an unknown address, we don't know the network and so just return a string
// indicating this.
//
// This part of the net.Addr interface.
func (o *OpaqueAddrs) Network() string {
	return "unknown network for unrecognized address type"
}
