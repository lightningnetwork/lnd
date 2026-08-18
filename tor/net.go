package tor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
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

// ClearNetConfig contains the options for clearnet connections. Its zero value
// selects direct connections with the default loopback bypass targets.
type ClearNetConfig struct {
	// SOCKS is the optional host:port of a SOCKS5 proxy.
	SOCKS string

	// Username and Password are the optional SOCKS5 credentials. A username
	// is required when a password is set.
	Username string
	Password string

	// NoProxyTargets contains additional destinations that should bypass
	// the SOCKS5 proxy. Entries can be exact hosts, wildcard zones such as
	// *.example.com, IP addresses, or CIDR networks.
	NoProxyTargets []string
}

// ClearNet is an implementation of the Net interface that defines behavior
// for regular network connections, optionally through a SOCKS5 proxy.
type ClearNet struct {
	socksAddr string
	auth      *proxy.Auth
	bypass    *bypassMatcher
}

// NewClearNet validates the configuration and returns a clearnet network. A
// configured SOCKS5 proxy is fail-closed: dialing errors are returned without
// falling back to a direct connection.
func NewClearNet(cfg ClearNetConfig) (*ClearNet, error) {
	bypass, err := newBypassMatcher(cfg.NoProxyTargets)
	if err != nil {
		return nil, err
	}

	if cfg.SOCKS == "" {
		if cfg.Username != "" || cfg.Password != "" {
			return nil, errors.New("SOCKS credentials require a proxy")
		}

		return &ClearNet{bypass: bypass}, nil
	}

	if err := validateSOCKSEndpoint(cfg.SOCKS); err != nil {
		return nil, err
	}
	if cfg.Username == "" && cfg.Password != "" {
		return nil, errors.New("SOCKS password requires a username")
	}
	if len(cfg.Username) > 255 || len(cfg.Password) > 255 {
		return nil, errors.New("SOCKS credentials exceed 255 bytes")
	}

	var auth *proxy.Auth
	if cfg.Username != "" {
		auth = &proxy.Auth{
			User:     cfg.Username,
			Password: cfg.Password,
		}
	}

	return &ClearNet{
		socksAddr: cfg.SOCKS,
		auth:      auth,
		bypass:    bypass,
	}, nil
}

// validateSOCKSEndpoint validates a SOCKS5 proxy endpoint without resolving
// its hostname.
func validateSOCKSEndpoint(endpoint string) error {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("invalid SOCKS5 proxy endpoint %q: %w",
			endpoint, err)
	}
	if host == "" {
		return fmt.Errorf("invalid SOCKS5 proxy endpoint %q: empty host",
			endpoint)
	}
	if net.ParseIP(host) == nil {
		if err := validateProxyHost(host); err != nil {
			return fmt.Errorf("invalid SOCKS5 proxy endpoint %q: %w",
				endpoint, err)
		}
	}

	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNum == 0 {
		return fmt.Errorf("invalid SOCKS5 proxy endpoint %q: invalid port",
			endpoint)
	}

	return nil
}

// proxyBypass returns the configured matcher or the loopback defaults for a
// zero-value ClearNet.
func (r *ClearNet) proxyBypass() *bypassMatcher {
	if r.bypass != nil {
		return r.bypass
	}

	return defaultBypassMatcher
}

// Dial connects directly or through the configured SOCKS5 proxy.
func (r *ClearNet) Dial(
	network, address string, timeout time.Duration) (net.Conn, error) {

	direct := &net.Dialer{Timeout: timeout}
	if r.socksAddr == "" {
		return direct.Dial(network, address)
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if r.proxyBypass().Match(host) {
		return direct.Dial(network, address)
	}

	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, errors.New("cannot dial non-tcp network via SOCKS5")
	}

	dialer, err := proxy.SOCKS5(
		"tcp", r.socksAddr, r.auth, direct,
	)
	if err != nil {
		return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
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

	// ClearNet is the network used for non-onion destinations when Tor is
	// skipped. If unset, direct clearnet is used for compatibility.
	ClearNet Net

	// NoProxyTargets contains validated destinations that should bypass Tor
	// and use ClearNet instead.
	NoProxyTargets []string

	bypass *bypassMatcher
}

// ProxyNetConfig contains the options for Tor connections and their clearnet
// routing policy.
type ProxyNetConfig struct {
	// SOCKS is the host:port of Tor's SOCKS5 proxy.
	SOCKS string

	// DNS is the host:port of the TCP DNS server used for SRV queries.
	DNS string

	// StreamIsolation randomizes Tor SOCKS5 credentials per connection.
	StreamIsolation bool

	// SkipProxyForClearNetTargets sends every non-onion destination through
	// ClearNet.
	SkipProxyForClearNetTargets bool

	// ClearNet handles destinations that bypass Tor.
	ClearNet Net

	// NoProxyTargets contains additional destinations that bypass Tor.
	NoProxyTargets []string
}

// NewProxyNet validates the proxy bypass targets and constructs a Tor network.
func NewProxyNet(cfg ProxyNetConfig) (*ProxyNet, error) {
	bypass, err := newBypassMatcher(cfg.NoProxyTargets)
	if err != nil {
		return nil, err
	}

	return &ProxyNet{
		SOCKS:                       cfg.SOCKS,
		DNS:                         cfg.DNS,
		StreamIsolation:             cfg.StreamIsolation,
		SkipProxyForClearNetTargets: cfg.SkipProxyForClearNetTargets,
		ClearNet:                    cfg.ClearNet,
		NoProxyTargets:              cfg.NoProxyTargets,
		bypass:                      bypass,
	}, nil
}

// clearNet returns the selected clearnet network.
func (p *ProxyNet) clearNet() Net {
	if p.ClearNet != nil {
		return p.ClearNet
	}

	return &ClearNet{}
}

// proxyBypass returns a validated matcher. Callers that accept user input
// should use NewProxyNet to surface validation errors during startup.
func (p *ProxyNet) proxyBypass() (*bypassMatcher, error) {
	if p.bypass != nil {
		return p.bypass, nil
	}

	return newBypassMatcher(p.NoProxyTargets)
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
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	// Onion services must always use Tor. For clearnet destinations, use
	// the selected clearnet network only when proxy skipping is enabled or
	// the destination explicitly matches a bypass rule.
	bypass, err := p.proxyBypass()
	if err != nil {
		return nil, err
	}
	if !IsOnionHost(host) && (p.SkipProxyForClearNetTargets ||
		bypass.Match(host)) {

		return p.clearNet().Dial(network, address, timeout)
	}

	return Dial(address, p.SOCKS, p.StreamIsolation, false, timeout)
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

	// SRV queries must always reach tor.dns over Tor, even if clearnet
	// proxy skipping is active or the DNS server matches a bypass rule.
	return LookupSRV(
		service, proto, name, p.SOCKS, p.DNS, p.StreamIsolation,
		false, timeout,
	)
}

// ResolveTCPAddr uses the Tor ResolveTCPAddr function in order to resolve TCP
// addresses over Tor.
func (p *ProxyNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	switch network {
	case "tcp", "tcp4", "tcp6", "onion":
	default:
		return nil, errors.New("cannot dial non-tcp network via Tor")
	}
	return ResolveTCPAddr(address, p.SOCKS)
}
