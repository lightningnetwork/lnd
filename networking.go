package main

import (
	"net"

	"github.com/lightningnetwork/lnd/torsvc"
)

// NetInterface is an interface housing a Dial function and several DNS functions.
type NetInterface interface {
	Dial(string, string) (net.Conn, error)
	LookupHost(string) ([]string, error)
	LookupSRV(string, string, string) (string, []*net.SRV, error)
	ResolveTCPAddr(string, string) (*net.TCPAddr, error)
}

// RegularNet is an implementation of NetInterface that uses the net package
// for everything.
type RegularNet struct{}

// TorProxyNet is an implementation of NetInterface that uses the torsvc module.
type TorProxyNet struct {
	TorSocks string
	TorDNS   string
}

// A compile time check to ensure RegularNet implements the NetInterface
// interface.
var _ NetInterface = (*RegularNet)(nil)

// A compile time check to ensure TorProxyNet implements the NetInterface
// interface.
var _ NetInterface = (*TorProxyNet)(nil)

// Dial is a wrapper of net.Dial
func (r *RegularNet) Dial(network, address string) (net.Conn, error) {
	return net.Dial(network, address)
}

// LookupHost is a wrapper of net.LookupHost
func (r *RegularNet) LookupHost(host string) ([]string, error) {
	return net.LookupHost(host)
}

// LookupSRV is a wrapper of net.LookupSRV
func (r *RegularNet) LookupSRV(service, proto, name string) (string, []*net.SRV,
	error) {
	return net.LookupSRV(service, proto, name)
}

// ResolveTCPAddr is a wrapper of net.ResolveTCPAddr
func (r *RegularNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr(network, address)
}

// Dial is a wrapper of torsvc.TorDial
func (t *TorProxyNet) Dial(network, address string) (net.Conn, error) {
	return torsvc.TorDial(address, t.TorSocks)
}

// LookupHost is a wrapper of torsvc.TorLookupHost
func (t *TorProxyNet) LookupHost(host string) ([]string, error) {
	return torsvc.TorLookupHost(host, t.TorSocks)
}

// LookupSRV is a wrapper of torsvc.TorLookupSRV
func (t *TorProxyNet) LookupSRV(service, proto, name string) (string, []*net.SRV,
	error) {
	return torsvc.TorLookupSRV(service, proto, name, t.TorSocks, t.TorDNS)
}

// ResolveTCPAddr is a wrapper of torsvc.TorResolveTCP
func (t *TorProxyNet) ResolveTCPAddr(network, address string) (*net.TCPAddr,
	error) {
	return torsvc.TorResolveTCP(address, t.TorSocks)
}
