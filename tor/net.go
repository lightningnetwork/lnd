package tor

import (
	"context"
	"errors"
	"net"
	"time"
)

// TODO: this interface and its implementations should ideally be moved
// elsewhere as they are not Tor-specific.

const (
	// DefaultConnTimeout is the maximum amount of time a dial will wait for
	// a connect to complete.
	DefaultConnTimeout time.Duration = time.Second * 120
)

// DialFunc is a type defines the signature of a dialer used by our Net
// interface.
type DialFunc func(net, addr string, timeout time.Duration) (net.Conn, error)

// Net is an interface housing a Dial function and several DNS functions that
// allows us to abstract the implementations of these functions over different
// networks, e.g. clearnet, Tor net, etc.
type Net interface {
	// Dial connects to the address on the named network.
	Dial(network, address string, timeout time.Duration) (net.Conn, error)

	// LookupHost performs DNS resolution on a given host and returns its
	// addresses.
	LookupHost(host string) ([]string, error)

	// LookupSRV tries to resolve an SRV query of the given service,
	// protocol, and domain name.
	LookupSRV(service, proto, name string,
		timeout time.Duration) (string, []*net.SRV, error)

	// ResolveTCPAddr resolves TCP addresses.
	ResolveTCPAddr(network, address string) (*net.TCPAddr, error)
}

// ClearNet is an implementation of the Net interface that defines behaviour
// for regular network connections.
type ClearNet struct{}

// Dial on the regular network uses net.Dial
func (r *ClearNet) Dial(
	network, address string, timeout time.Duration) (net.Conn, error) {

	return net.DialTimeout(network, address, timeout)
}

// LookupHost for regular network uses the net.LookupHost function
func (r *ClearNet) LookupHost(host string) ([]string, error) {
	return net.LookupHost(host)
}

// LookupSRV for regular network uses net.LookupSRV function
func (r *ClearNet) LookupSRV(service, proto, name string,
	timeout time.Duration) (string, []*net.SRV, error) {

	// Create a context with a timeout value.
	ctxt, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return net.DefaultResolver.LookupSRV(ctxt, service, proto, name)
}

// ResolveTCPAddr for regular network uses net.ResolveTCPAddr function
func (r *ClearNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr(network, address)
}

// ProxyNet is an implementation of the Net interface that defines behavior
// for Tor network connections.
type ProxyNet struct {
	// SOCKS is the host:port which Tor's exposed SOCKS5 proxy is listening
	// on.
	SOCKS string

	// DNS is the host:port of the DNS server for Tor to use for SRV
	// queries.
	DNS string

	// StreamIsolation is a bool that determines if we should force the
	// creation of a new circuit for this connection. If true, then this
	// means that our traffic may be harder to correlate as each connection
	// will now use a distinct circuit.
	StreamIsolation bool

	// SkipProxyForClearNetTargets allows the proxy network to use direct
	// connections to non-onion service targets. If enabled, the node IP
	// address will be revealed while communicating with such targets.
	SkipProxyForClearNetTargets bool
}

// Dial uses the Tor Dial function in order to establish connections through
// Tor. Since Tor only supports TCP connections, only TCP networks are allowed.
func (p *ProxyNet) Dial(network, address string,
	timeout time.Duration) (net.Conn, error) {

	switch network {
	case "tcp", "tcp4", "tcp6", "onion":
	default:
		return nil, errors.New("cannot dial non-tcp network via Tor")
	}
	return Dial(
		address, p.SOCKS, p.StreamIsolation,
		p.SkipProxyForClearNetTargets, timeout,
	)
}

// skipProxy reports whether a lookup or connection for host should bypass Tor,
// which is the case for clearnet hosts once SkipProxyForClearNetTargets is set.
// Dial applies the same rule, so a host we'd connect to directly is also
// resolved directly.
func (p *ProxyNet) skipProxy(host string) bool {
	return p.SkipProxyForClearNetTargets && !IsOnionHost(host)
}

// LookupHost uses the Tor LookupHost function in order to resolve hosts over
// Tor, unless SkipProxyForClearNetTargets is set and the host isn't an onion
// host, in which case the host is resolved directly.
func (p *ProxyNet) LookupHost(host string) ([]string, error) {
	if p.skipProxy(host) {
		return (&ClearNet{}).LookupHost(host)
	}

	return LookupHost(host, p.SOCKS)
}

// LookupSRV uses the Tor LookupSRV function in order to resolve SRV DNS queries
// over Tor.
func (p *ProxyNet) LookupSRV(service, proto,
	name string, timeout time.Duration) (string, []*net.SRV, error) {

	return LookupSRV(
		service, proto, name, p.SOCKS, p.DNS, p.StreamIsolation,
		p.SkipProxyForClearNetTargets, timeout,
	)
}

// ResolveTCPAddr uses the Tor ResolveTCPAddr function in order to resolve TCP
// addresses over Tor, unless SkipProxyForClearNetTargets is set and the host
// isn't an onion host, in which case the address is resolved directly.
func (p *ProxyNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	switch network {
	case "tcp", "tcp4", "tcp6", "onion":
	default:
		return nil, errors.New("cannot dial non-tcp network via Tor")
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if p.skipProxy(host) {
		if network == "onion" {
			network = "tcp"
		}

		return (&ClearNet{}).ResolveTCPAddr(network, address)
	}

	return ResolveTCPAddr(address, p.SOCKS)
}
