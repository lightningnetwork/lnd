package main

import (
	"net"

	"github.com/lightningnetwork/lnd/torsvc"
)

type NetInterface interface {
	Dial(string, string) (net.Conn, error)
	LookupHost(string) ([]string, error)
	LookupSRV(string, string, string) (string, []*net.SRV, error)
	ResolveTCPAddr(string, string) (*net.TCPAddr, error)
}

// An implementation of NetInterface that uses the net package for everything.
type RegularNet struct {}

// An implementation of NetInterface that uses the torsvc module.
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

func (r *RegularNet) Dial(network, address string) (net.Conn, error) {
	return net.Dial(network, address)
}

func (r *RegularNet) LookupHost(host string) ([]string, error) {
	return net.LookupHost(host)
}

func (r *RegularNet) LookupSRV(service, proto, name string) (string, []*net.SRV,
	error) {
	return net.LookupSRV(service, proto, name)
}

func (r *RegularNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr(network, address)
}

func (t *TorProxyNet) Dial(network, address string) (net.Conn, error) {
	return torsvc.TorDial(address, t.TorSocks)
}

func (t *TorProxyNet) LookupHost(host string) ([]string, error) {
	return torsvc.TorLookupHost(host, t.TorSocks)
}

func (t *TorProxyNet) LookupSRV(service, proto, name string) (string, []*net.SRV,
	error) {
	return torsvc.TorLookupSRV(service, proto, name, t.TorSocks, t.TorDNS)
}

func (t *TorProxyNet) ResolveTCPAddr(network, address string) (*net.TCPAddr,
	error) {
	return torsvc.TorResolveTCP(address, t.TorSocks)
}
