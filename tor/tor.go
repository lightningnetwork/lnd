package tor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"

	"github.com/btcsuite/btcd/connmgr"
	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
)

var (
	// dnsCodes maps the DNS response codes to a friendly description. This
	// does not include the BADVERS code because of duplicate keys and the
	// underlying DNS (miekg/dns) package not using it. For more info, see
	// https://www.iana.org/assignments/dns-parameters/dns-parameters.xhtml.
	dnsCodes = map[int]string{
		0:  "no error",
		1:  "format error",
		2:  "server failure",
		3:  "non-existent domain",
		4:  "not implemented",
		5:  "query refused",
		6:  "name exists when it should not",
		7:  "RR set exists when it should not",
		8:  "RR set that should exist does not",
		9:  "server not authoritative for zone",
		10: "name not contained in zone",
		16: "TSIG signature failure",
		17: "key not recognized",
		18: "signature out of time window",
		19: "bad TKEY mode",
		20: "duplicate key name",
		21: "algorithm not supported",
		22: "bad truncation",
		23: "bad/missing server cookie",
	}
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
func Dial(address, socksAddr string, streamIsolation bool) (net.Conn, error) {
	conn, err := dial(address, socksAddr, streamIsolation)
	if err != nil {
		return nil, err
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

// dial establishes a connection to the address via Tor's SOCKS proxy. Only TCP
// is supported over Tor. The final argument determines if we should force
// stream isolation for this new connection. If we do, then this means this new
// connection will use a fresh circuit, rather than possibly re-using an
// existing circuit.
func dial(address, socksAddr string, streamIsolation bool) (net.Conn, error) {
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

	// Establish the connection through Tor's SOCKS proxy.
	dialer, err := proxy.SOCKS5("tcp", socksAddr, auth, proxy.Direct)
	if err != nil {
		return nil, err
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
func LookupSRV(service, proto, name, socksAddr, dnsServer string,
	streamIsolation bool) (string, []*net.SRV, error) {

	// Connect to the DNS server we'll be using to query SRV records.
	conn, err := dial(dnsServer, socksAddr, streamIsolation)
	if err != nil {
		return "", nil, err
	}

	dnsConn := &dns.Conn{Conn: conn}
	defer dnsConn.Close()

	// Once connected, we'll construct the SRV request for the host
	// following the format _service._proto.name. as described in RFC #2782.
	host := fmt.Sprintf("_%s._%s.%s.", service, proto, name)
	msg := new(dns.Msg).SetQuestion(host, dns.TypeSRV)

	// Send the request to the DNS server and read its response.
	if err := dnsConn.WriteMsg(msg); err != nil {
		return "", nil, err
	}
	resp, err := dnsConn.ReadMsg()
	if err != nil {
		return "", nil, err
	}

	// We'll fail if we were unable to query the DNS server for our record.
	if resp.Rcode != dns.RcodeSuccess {
		return "", nil, fmt.Errorf("unable to query for SRV records: "+
			"%s", dnsCodes[resp.Rcode])
	}

	// Retrieve the RR(s) of the Answer section.
	var rrs []*net.SRV
	for _, rr := range resp.Answer {
		srv := rr.(*dns.SRV)
		rrs = append(rrs, &net.SRV{
			Target:   srv.Target,
			Port:     srv.Port,
			Priority: srv.Priority,
			Weight:   srv.Weight,
		})
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

	ip, err := LookupHost(host, socksAddr)
	if err != nil {
		return nil, err
	}

	p, err := strconv.Atoi(port)
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
