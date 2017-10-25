package lnnet

import (
	"fmt"
	"net"

	"github.com/lightningnetwork/lnd/torsvc"
)

// LnNet is an implementation of LightningNet that abstracts away the "net"
// and "torsvc" functions from the user.
type LnNet struct {
	Tor      bool
	TorDNS   string
	TorSocks string
}

// A compile time check to ensure LnNet implements the LightningNet interface.
var _ LightningNet = (*LnNet)(nil)

// Dial uses either the "net" or "torsvc" dial function.
func (l *LnNet) Dial(network, address string) (net.Conn, error) {
	if l.Tor {
		if network != "tcp" {
			return nil, fmt.Errorf("Cannot dial non-tcp network via Tor")
		}
		return torsvc.TorDial(address, l.TorSocks)
	}
	return net.Dial(network, address)
}

// LookupHost uses either the "net" or "torsvc LookupHost function.
func (l *LnNet) LookupHost(host string) ([]string, error) {
	if l.Tor {
		return torsvc.TorLookupHost(host, l.TorSocks)
	}
	return net.LookupHost(host)
}

// LookupSRV uses either the "net" or "torsvc" LookupSRV function.
func (l *LnNet) LookupSRV(service, proto, name string) (string, []*net.SRV, error) {
	if l.Tor {
		return torsvc.TorLookupSRV(service, proto, name, l.TorSocks, l.TorDNS)
	}
	return net.LookupSRV(service, proto, name)
}

// ResolveTCPAddr uses either the "net" or "torsvc" ResolveTCP function.
func (l *LnNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	if l.Tor {
		if network != "tcp" {
			return nil, fmt.Errorf("Cannot dial non-tcp network via Tor")
		}
		return torsvc.TorResolveTCP(address, l.TorSocks)
	}
	return net.ResolveTCPAddr(network, address)
}
