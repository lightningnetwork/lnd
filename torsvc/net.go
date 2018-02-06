package torsvc

import (
	"fmt"
	"net"
)

// RegularNet is an implementation of the Net interface that defines behaviour
// for Regular network connections
type RegularNet struct{}

// Dial on the regular network uses net.Dial
func (r *RegularNet) Dial(network, address string) (net.Conn, error) {
	return net.Dial(network, address)
}

// LookupHost for regular network uses the net.LookupHost function
func (r *RegularNet) LookupHost(host string) ([]string, error) {
	return net.LookupHost(host)
}

// LookupSRV for regular network uses net.LookupSRV function
func (r *RegularNet) LookupSRV(service, proto, name string) (string, []*net.SRV, error) {
	return net.LookupSRV(service, proto, name)
}

// ResolveTCPAddr for regular network uses net.ResolveTCPAddr function
func (r *RegularNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr(network, address)
}

// TorProxyNet is an implementation of the Net interface that defines behaviour
// for Tor network connections
type TorProxyNet struct {
	// TorDNS is the IP:PORT of the DNS server for Tor to use for SRV queries
	TorDNS string

	// TorSocks is the port which Tor's exposed SOCKS5 proxy is listening on.
	// This is used for an outbound-only mode, so the node will not listen for
	// incoming connections
	TorSocks string

	// StreamIsolation is a bool that determines if we should force the
	// creation of a new circuit for this connection. If true, then this
	// means that our traffic may be harder to correlate as each connection
	// will now use a distinct circuit.
	StreamIsolation bool
}

// Dial on the Tor network uses the torsvc TorDial() function, and requires
// that network specified be tcp because only that is supported
func (t *TorProxyNet) Dial(network, address string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("Cannot dial non-tcp network via Tor")
	}
	return TorDial(address, t.TorSocks, t.StreamIsolation)
}

// LookupHost on Tor network uses the torsvc TorLookupHost function.
func (t *TorProxyNet) LookupHost(host string) ([]string, error) {
	return TorLookupHost(host, t.TorSocks)
}

// LookupSRV on Tor network uses the torsvc TorLookupHost function.
func (t *TorProxyNet) LookupSRV(service, proto, name string) (string, []*net.SRV, error) {
	return TorLookupSRV(service, proto, name, t.TorSocks, t.TorDNS)
}

// ResolveTCPAddr on Tor network uses the towsvc TorResolveTCP function, and
// requires network to be "tcp" because only "tcp" is supported
func (t *TorProxyNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("Cannot dial non-tcp network via Tor")
	}
	return TorResolveTCP(address, t.TorSocks)
}
