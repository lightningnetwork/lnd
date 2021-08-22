package tor

import (
	"context"
	"errors"
	"net"
	"time"

	"golang.org/x/net/proxy"
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

type Dialer interface {
	Dial(network, address string) (net.Conn, error)
}

// Net is an interface housing a Dial function and several DNS functions that
// allows us to abstract the implementations of these functions over different
// networks, e.g. clearnet, Tor net, etc.
type Net interface {
	createDialer(auth *proxy.Auth, timeout time.Duration) (Dialer, error)
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
type ClearNet struct {
	// SOCKS is the host:port for SOCKS5 proxy to use for clearnet connections.
	SOCKS string

	// NoProxyTargets is a string of comma-separated values
	// specifying hosts that should bypass the proxy. Each value is either an
	// IP address, a CIDR range, a zone (*.example.com) or a host name
	// (localhost). A best effort is made to parse the string and errors are
	// ignored.
	NoProxyTargets string
}

func (r *ClearNet) createDialer(auth *proxy.Auth, timeout time.Duration) (Dialer, error) {
	clearDialer := &net.Dialer{Timeout: timeout}
	if r.SOCKS == "" {
		return clearDialer, nil
	}
	dialer, err := proxy.SOCKS5("tcp", r.SOCKS, auth, clearDialer)
	if err != nil {
		return nil, err
	}
	perHostDialer := proxy.NewPerHost(dialer, clearDialer)
	perHostDialer.AddFromString(r.NoProxyTargets)
	return perHostDialer, nil
}

// Dial on the regular network uses net.Dial
func (r *ClearNet) Dial(
	network, address string, timeout time.Duration) (net.Conn, error) {

	dialer, err := r.createDialer(nil, timeout)
	if err != nil {
		return nil, err
	}
	return dialer.Dial(network, address)
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
	// SOCKS is the host:port for SOCKS5 proxy to use for clearnet connections.
	SOCKS string

	// DNS is the host:port of the DNS server for Tor to use for SRV
	// queries.
	DNS string

	// StreamIsolation is a bool that determines if we should force the
	// creation of a new circuit for this connection. If true, then this
	// means that our traffic may be harder to correlate as each connection
	// will now use a distinct circuit.
	StreamIsolation bool

	// SkipProxyForClearNetTargets forces the proxy network to use direct
	// connections for all non-onion service targets. If enabled, the node IP
	// address will be revealed while communicating with such targets.

	// NoProxyTargets is a string of comma-separated values
	// specifying hosts that should bypass the proxy. Each value is either an
	// IP address, a CIDR range, a zone (*.example.com) or a host name
	// (localhost). A best effort is made to parse the string and errors are
	// ignored.
	NoProxyTargets string
	// Configuration to use for clearnet connections
	ClearNet Net
}

// Dial uses the Tor Dial function in order to establish connections through
// Tor. Since Tor only supports TCP connections, only TCP networks are allowed.
func (p *ProxyNet) Dial(network, address string,
	timeout time.Duration) (net.Conn, error) {

	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, errors.New("cannot dial non-tcp network via Tor")
	}

	conn, err := dial(address, p, timeout)
	if err != nil {
		return nil, err
	}

	// Now that the connection is established, we'll create our internal
	// proxyConn that will serve in populating the correct remote address
	// of the connection, rather than using the proxy's address.
	remoteAddr, err := ParseAddr(address, p.SOCKS)
	if err != nil {
		return nil, err
	}

	return &proxyConn{
		Conn:       conn,
		remoteAddr: remoteAddr,
	}, nil

}

// LookupHost uses the Tor LookupHost function in order to resolve hosts over
// Tor.
func (p *ProxyNet) LookupHost(host string) ([]string, error) {
	return LookupHost(host, p.SOCKS)
}

// LookupSRV uses the Tor LookupSRV function in order to resolve SRV DNS queries
// over Tor.
func (p *ProxyNet) LookupSRV(service, proto,
	name string, timeout time.Duration) (string, []*net.SRV, error) {

	return LookupSRV(service, proto, name, p, timeout)
}

// ResolveTCPAddr uses the Tor ResolveTCPAddr function in order to resolve TCP
// addresses over Tor.
func (p *ProxyNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, errors.New("cannot dial non-tcp network via Tor")
	}
	return ResolveTCPAddr(address, p.SOCKS)
}
