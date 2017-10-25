package torsvc

import (
	"fmt"
	"net"
)

// MultiNet is an implementation of the Net interface that abstracts away the
// "net" and "torsvc" functions from the user. This can be used to "switch"
// Tor functionality on/off based on the MultiNet.Tor boolean. MultiNet allows
// for callers to call net functions when necessary and torsvc functions when
// necessary
type MultiNet struct {
	Tor      bool
	TorDNS   string
	TorSocks string
}

// A compile time check to ensure MultiNet implements the Net interface.
var _ Net = (*MultiNet)(nil)

// Dial uses either the "net" or "torsvc" dial function.
func (m *MultiNet) Dial(network, address string) (net.Conn, error) {
	if m.Tor {
		if network != "tcp" {
			return nil, fmt.Errorf("Cannot dial non-tcp network via Tor")
		}
		return TorDial(address, m.TorSocks)
	}
	return net.Dial(network, address)
}

// LookupHost uses either the "net" or "torsvc LookupHost function.
func (m *MultiNet) LookupHost(host string) ([]string, error) {
	if m.Tor {
		return TorLookupHost(host, m.TorSocks)
	}
	return net.LookupHost(host)
}

// LookupSRV uses either the "net" or "torsvc" LookupSRV function.
func (m *MultiNet) LookupSRV(service, proto, name string) (string, []*net.SRV, error) {
	if m.Tor {
		return TorLookupSRV(service, proto, name, m.TorSocks, m.TorDNS)
	}
	return net.LookupSRV(service, proto, name)
}

// ResolveTCPAddr uses either the "net" or "torsvc" ResolveTCP function.
func (m *MultiNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	if m.Tor {
		if network != "tcp" {
			return nil, fmt.Errorf("Cannot dial non-tcp network via Tor")
		}
		return TorResolveTCP(address, m.TorSocks)
	}
	return net.ResolveTCPAddr(network, address)
}
