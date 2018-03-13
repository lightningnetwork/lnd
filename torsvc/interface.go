package torsvc

import (
	"net"
)

// Net is an interface housing a Dial function and several DNS functions, to
// abstract the implementation of these functions over both Regular and Tor
type Net interface {
	// Dial accepts a network and address and returns a connection to a remote
	// peer.
	Dial(string, string) (net.Conn, error)

	// LookupHost performs DNS resolution on a given hostname and returns
	// addresses of that hostname
	LookupHost(string) ([]string, error)

	// LookupSRV allows a service and network to be specified and makes queries
	// to a given DNS server for SRV queries.
	LookupSRV(string, string, string) (string, []*net.SRV, error)

	// ResolveTCPAddr is a used to resolve publicly advertised TCP addresses.
	ResolveTCPAddr(string, string) (*net.TCPAddr, error)
}
