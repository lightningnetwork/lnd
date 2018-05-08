package torsvc

import (
	"fmt"
	"net"
	"strconv"

	"github.com/btcsuite/go-socks/socks"
	"github.com/miekg/dns"
	"github.com/roasbeef/btcd/connmgr"
	"golang.org/x/net/proxy"
)

const (
	localhost = "127.0.0.1"
)

var (
	// DNS Message Response Codes, see
	// https://www.iana.org/assignments/dns-parameters/dns-parameters.xhtml
	dnsCodes = map[int]string{
		0:  "No Error",
		1:  "Format Error",
		2:  "Server Failure",
		3:  "Non-Existent Domain",
		4:  "Not Implemented",
		5:  "Query Refused",
		6:  "Name Exists when it should not",
		7:  "RR Set Exists when it should not",
		8:  "RR Set that should exist does not",
		9:  "Server Not Authoritative for zone",
		10: "Name not contained in zone",
		// Left out 16: "Bad OPT Version" because of duplicate keys and
		// because miekg/dns does not use this message response code.
		16: "TSIG Signature Failure",
		17: "Key not recognized",
		18: "Signature out of time window",
		19: "Bad TKEY Mode",
		20: "Duplicate key name",
		21: "Algorithm not supported",
		22: "Bad Truncation",
		23: "Bad/missing Server Cookie",
	}
)

// TorDial returns a connection to a remote peer via Tor's socks proxy. Only
// TCP is supported over Tor. The final argument determines if we should force
// stream isolation for this new connection. If we do, then this means this new
// connection will use a fresh circuit, rather than possibly re-using an
// existing circuit.
func TorDial(address, socksPort string, streamIsolation bool) (net.Conn, error) {
	p := &socks.Proxy{
		Addr:         localhost + ":" + socksPort,
		TorIsolation: streamIsolation,
	}

	return p.Dial("tcp", address)
}

// TorLookupHost performs DNS resolution on a given hostname via Tor's
// native resolver. Only IPv4 addresses are returned.
func TorLookupHost(host, socksPort string) ([]string, error) {
	ip, err := connmgr.TorLookupIP(host, localhost+":"+socksPort)
	if err != nil {
		return nil, err
	}

	var addrs []string
	// Only one IPv4 address is returned by the TorLookupIP function.
	addrs = append(addrs, ip[0].String())
	return addrs, nil
}

// TorLookupSRV uses Tor's socks proxy to route DNS SRV queries. Tor does not
// natively support SRV queries so we must route all SRV queries THROUGH the
// Tor proxy and connect directly to a DNS server and query it.
// NOTE: TorLookupSRV uses golang's proxy package since go-socks will cause
// the SRV request to hang.
func TorLookupSRV(service, proto, name, socksPort, dnsServer string) (string,
	[]*net.SRV, error) {
	// _service._proto.name as described in RFC#2782.
	host := "_" + service + "._" + proto + "." + name + "."

	// Set up golang's proxy dialer - Tor's socks proxy doesn't support
	// authentication.
	dialer, err := proxy.SOCKS5(
		"tcp",
		localhost+":"+socksPort,
		nil,
		proxy.Direct,
	)
	if err != nil {
		return "", nil, err
	}

	// Dial dnsServer via Tor. dnsServer must have TCP resolution enabled
	// for the port we are dialing.
	conn, err := dialer.Dial("tcp", dnsServer)
	if err != nil {
		return "", nil, err
	}

	// Construct the actual SRV request.
	msg := new(dns.Msg)
	msg.SetQuestion(host, dns.TypeSRV)
	msg.RecursionDesired = true

	dnsConn := &dns.Conn{Conn: conn}
	defer dnsConn.Close()

	// Write the SRV request.
	dnsConn.WriteMsg(msg)

	// Read the response.
	resp, err := dnsConn.ReadMsg()
	if err != nil {
		return "", nil, err
	}

	// If the message response code was not the success code, fail.
	if resp.Rcode != dns.RcodeSuccess {
		return "", nil, fmt.Errorf("Unsuccessful SRV request, "+
			"received: %s", dnsCodes[resp.Rcode])
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

// TorResolveTCP uses Tor's proxy to resolve TCP addresses instead of the
// system resolver that ResolveTCPAddr and related functions use. Only TCP
// resolution is supported.
func TorResolveTCP(address, socksPort string) (*net.TCPAddr, error) {
	// Split host:port since the lookup function does not take a port.
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	// Look up the host's IP address via Tor.
	ip, err := TorLookupHost(host, socksPort)
	if err != nil {
		return nil, err
	}

	// Convert port to an int.
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}

	// Return a *net.TCPAddr exactly like net.ResolveTCPAddr.
	return &net.TCPAddr{
		IP:   net.ParseIP(ip[0]),
		Port: p,
	}, nil
}
