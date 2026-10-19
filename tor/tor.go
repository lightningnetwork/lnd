package tor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/connmgr"
	"github.com/lightningnetwork/lnd/tor/dnsclient"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/proxy"
)

// proxyConn is a wrapper around net.Conn that allows us to expose the actual
// remote address we're dialing, rather than the proxy's address.
type proxyConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (c *proxyConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// Dial is a wrapper over the non-exported dial function that returns a wrapper
// around net.Conn in order to expose the actual remote address we're dialing,
// rather than the proxy's address.
func Dial(address, socksAddr string, streamIsolation bool,
	skipProxyForClearNetTargets bool,
	timeout time.Duration) (net.Conn, error) {

	conn, err := dialProxy(
		address, socksAddr, streamIsolation,
		skipProxyForClearNetTargets, timeout,
	)
	if err != nil {
		return nil, fmt.Errorf("dial proxy failed: %w", err)
	}

	// Now that the connection is established, we'll create our internal
	// proxyConn that will serve in populating the correct remote address
	// of the connection, rather than using the proxy's address.
	remoteAddr, err := ParseAddr(address, socksAddr)
	if err != nil {
		return nil, err
	}

	return &proxyConn{
		Conn:       conn,
		remoteAddr: remoteAddr,
	}, nil
}

// dialProxy establishes a connection to the address via the provided TOR SOCKS
// proxy. Only TCP traffic may be routed via Tor.
//
// streamIsolation determines if we should force stream isolation for this new
// connection. If enabled, new connections will use a fresh circuit, rather than
// possibly re-using an existing circuit.
//
// skipProxyForClearNetTargets argument allows the dialer to directly connect
// to the provided address if it does not represent an union service, skipping
// the SOCKS proxy.
func dialProxy(address, socksAddr string, streamIsolation bool,
	skipProxyForClearNetTargets bool,
	timeout time.Duration) (net.Conn, error) {

	// If we were requested to force stream isolation for this connection,
	// we'll populate the authentication credentials with random data as
	// Tor will create a new circuit for each set of credentials.
	var auth *proxy.Auth
	if streamIsolation {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}

		auth = &proxy.Auth{
			User:     hex.EncodeToString(b[:8]),
			Password: hex.EncodeToString(b[8:]),
		}
	}

	clearDialer := &net.Dialer{Timeout: timeout}
	if skipProxyForClearNetTargets {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}

		// The SOCKS proxy is skipped if the target
		// is not an union address.
		if !IsOnionHost(host) {
			return clearDialer.Dial("tcp", address)
		}
	}

	// Establish the connection through Tor's SOCKS proxy.
	dialer, err := proxy.SOCKS5("tcp", socksAddr, auth, clearDialer)
	if err != nil {
		return nil, fmt.Errorf("establish sock proxy: %w", err)
	}

	return dialer.Dial("tcp", address)
}

// LookupHost performs DNS resolution on a given host via Tor's native resolver.
// Only IPv4 addresses are returned.
func LookupHost(host, socksAddr string) ([]string, error) {
	ip, err := connmgr.TorLookupIP(host, socksAddr)
	if err != nil {
		return nil, err
	}

	// Only one IPv4 address is returned by the TorLookupIP function.
	return []string{ip[0].String()}, nil
}

// LookupSRV uses Tor's SOCKS proxy to route DNS SRV queries. Tor does not
// natively support SRV queries so we must route all SRV queries through the
// proxy by connecting directly to a DNS server and querying it. The DNS server
// must have TCP resolution enabled for the given port.
func LookupSRV(service, proto, name, socksAddr,
	dnsServer string, streamIsolation bool, skipProxyForClearNetTargets bool,
	timeout time.Duration) (string, []*net.SRV, error) {

	// Connect to the DNS server we'll be using to query SRV records.
	conn, err := dialProxy(
		dnsServer, socksAddr, streamIsolation,
		skipProxyForClearNetTargets, timeout,
	)
	if err != nil {
		return "", nil, err
	}

	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", nil, fmt.Errorf("set DNS query deadline: %w", err)
	}

	// Once connected, we'll construct the SRV request for the host
	// following the format _service._proto.name. as described in RFC #2782.
	host := fmt.Sprintf("_%s._%s.%s.", service, proto, name)
	rrs, rcode, err := dnsclient.QuerySRV(conn, host)
	if err != nil {
		return "", nil, err
	}

	// We'll fail if we were unable to query the DNS server for our record.
	if rcode != dnsmessage.RCodeSuccess {
		return "", nil, fmt.Errorf("unable to query for SRV records: "+
			"%s", dnsclient.RCodeText(rcode))
	}

	return "", rrs, nil
}

// ResolveTCPAddr uses Tor's proxy to resolve TCP addresses instead of the
// standard system resolver provided in the `net` package.
func ResolveTCPAddr(address, socksAddr string) (*net.TCPAddr, error) {
	// Split host:port since the lookup function does not take a port.
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}

	// Do we already have an IP? Then we don't need to look up anything.
	if ip := net.ParseIP(host); ip != nil {
		return &net.TCPAddr{
			IP:   ip,
			Port: p,
		}, nil
	}

	ip, err := LookupHost(host, socksAddr)
	if err != nil {
		return nil, err
	}

	return &net.TCPAddr{
		IP:   net.ParseIP(ip[0]),
		Port: p,
	}, nil
}

// ParseAddr parses an address from its string format to a net.Addr.
func ParseAddr(address, socksAddr string) (net.Addr, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}

	if IsOnionHost(host) {
		return &OnionAddr{OnionService: host, Port: port}, nil
	}

	return ResolveTCPAddr(address, socksAddr)
}

// IsOnionHost determines whether a host is part of an onion address.
func IsOnionHost(host string) bool {
	// Note the starting index of the onion suffix in the host depending
	// on its length.
	var suffixIndex int
	switch len(host) {
	case V2Len:
		suffixIndex = V2Len - OnionSuffixLen
	case V3Len:
		suffixIndex = V3Len - OnionSuffixLen
	default:
		return false
	}

	// Make sure the host ends with the ".onion" suffix.
	if host[suffixIndex:] != OnionSuffix {
		return false
	}

	// We'll now attempt to decode the host without its suffix, as the
	// suffix includes invalid characters. This will tell us if the host is
	// actually valid if successful.
	host = host[:suffixIndex]
	if _, err := Base32Encoding.DecodeString(host); err != nil {
		return false
	}

	return true
}
