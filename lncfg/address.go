package lncfg

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/tor"
)

var (
	loopBackAddrs = []string{"localhost", "127.0.0.1", "[::1]"}
)

type tcpResolver = func(network, addr string) (*net.TCPAddr, error)

// NormalizeAddresses returns a new slice with all the passed addresses
// normalized with the given default port and all duplicates removed.
func NormalizeAddresses(addrs []string, defaultPort string,
	tcpResolver tcpResolver) ([]net.Addr, error) {

	result := make([]net.Addr, 0, len(addrs))
	seen := map[string]struct{}{}

	for _, addr := range addrs {
		parsedAddr, err := ParseAddressString(
			addr, defaultPort, tcpResolver,
		)
		if err != nil {
			return nil, err
		}

		if _, ok := seen[parsedAddr.String()]; !ok {
			result = append(result, parsedAddr)
			seen[parsedAddr.String()] = struct{}{}
		}
	}

	return result, nil
}

// EnforceSafeAuthentication enforces "safe" authentication taking into account
// the interfaces that the RPC servers are listening on, and if macaroons are
// activated or not. To protect users from using dangerous config combinations,
// we'll prevent disabling authentication if the server is listening on a public
// interface.
func EnforceSafeAuthentication(addrs []net.Addr, macaroonsActive bool) error {
	// We'll now examine all addresses that this RPC server is listening
	// on. If it's a localhost address, we'll skip it, otherwise, we'll
	// return an error if macaroons are inactive.
	for _, addr := range addrs {
		if IsLoopback(addr.String()) || IsUnix(addr) {
			continue
		}

		if !macaroonsActive {
			return fmt.Errorf("Detected RPC server listening on "+
				"publicly reachable interface %v with "+
				"authentication disabled! Refusing to start "+
				"with --no-macaroons specified.", addr)
		}
	}

	return nil
}

// ListenOnAddress creates a listener that listens on the given address.
func ListenOnAddress(addr net.Addr) (net.Listener, error) {
	return net.Listen(addr.Network(), addr.String())
}

// TLSListenOnAddress creates a TLS listener that listens on the given address.
func TLSListenOnAddress(addr net.Addr,
	config *tls.Config) (net.Listener, error) {
	return tls.Listen(addr.Network(), addr.String(), config)
}

// IsLoopback returns true if an address describes a loopback interface.
func IsLoopback(addr string) bool {
	for _, loopback := range loopBackAddrs {
		if strings.Contains(addr, loopback) {
			return true
		}
	}

	return false
}

// IsUnix returns true if an address describes an Unix socket address.
func IsUnix(addr net.Addr) bool {
	return strings.HasPrefix(addr.Network(), "unix")
}

// ParseAddressString converts an address in string format to a net.Addr that is
// compatible with lnd. UDP is not supported because lnd needs reliable
// connections. We accept a custom function to resolve any TCP addresses so
// that caller is able control exactly how resolution is performed.
func ParseAddressString(strAddress string, defaultPort string,
	tcpResolver tcpResolver) (net.Addr, error) {

	var parsedNetwork, parsedAddr string

	// Addresses can either be in network://address:port format,
	// network:address:port, address:port, or just port. We want to support
	// all possible types.
	if strings.Contains(strAddress, "://") {
		parts := strings.Split(strAddress, "://")
		parsedNetwork, parsedAddr = parts[0], parts[1]
	} else if strings.Contains(strAddress, ":") {
		parts := strings.Split(strAddress, ":")
		parsedNetwork = parts[0]
		parsedAddr = strings.Join(parts[1:], ":")
	}

	// Only TCP and Unix socket addresses are valid. We can't use IP or
	// UDP only connections for anything we do in lnd.
	switch parsedNetwork {
	case "unix", "unixpacket":
		return net.ResolveUnixAddr(parsedNetwork, parsedAddr)

	case "tcp", "tcp4", "tcp6":
		return tcpResolver(
			parsedNetwork, verifyPort(parsedAddr, defaultPort),
		)

	case "ip", "ip4", "ip6", "udp", "udp4", "udp6", "unixgram":
		return nil, fmt.Errorf("only TCP or unix socket "+
			"addresses are supported: %s", parsedAddr)

	default:
		// We'll now possibly apply the default port, use the local
		// host short circuit, or parse out an all interfaces listen.
		addrWithPort := verifyPort(strAddress, defaultPort)
		rawHost, rawPort, _ := net.SplitHostPort(addrWithPort)

		// If we reach this point, then we'll check to see if we have
		// an onion addresses, if so, we can directly pass the raw
		// address and port to create the proper address.
		if tor.IsOnionHost(rawHost) {
			portNum, err := strconv.Atoi(rawPort)
			if err != nil {
				return nil, err
			}

			return &tor.OnionAddr{
				OnionService: rawHost,
				Port:         portNum,
			}, nil
		}

		// Otherwise, we'll attempt the resolve the host. The Tor
		// resolver is unable to resolve local addresses, so we'll use
		// the system resolver instead.
		if rawHost == "" || IsLoopback(rawHost) {
			return net.ResolveTCPAddr("tcp", addrWithPort)
		}

		return tcpResolver("tcp", addrWithPort)
	}
}

// verifyPort makes sure that an address string has both a host and a port. If
// there is no port found, the default port is appended. If the address is just
// a port, then we'll assume that the user is using the short cut to specify a
// localhost:port address.
func verifyPort(address string, defaultPort string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		// If the address itself is just an integer, then we'll assume
		// that we're mapping this directly to a localhost:port pair.
		// This ensures we maintain the legacy behavior.
		if _, err := strconv.Atoi(address); err == nil {
			return net.JoinHostPort("localhost", address)
		}

		// Otherwise, we'll assume that the address just failed to
		// attach its own port, so we'll use the default port. In the
		// case of IPv6 addresses, if the host is already surrounded by
		// brackets, then we'll avoid using the JoinHostPort function,
		// since it will always add a pair of brackets.
		if strings.HasPrefix(address, "[") {
			return address + ":" + defaultPort
		}
		return net.JoinHostPort(address, defaultPort)
	}

	// In the case that both the host and port are empty, we'll use the
	// default port.
	if host == "" && port == "" {
		return ":" + defaultPort
	}

	return address
}

// ClientAddressDialer creates a gRPC dialer that can also dial unix socket
// addresses instead of just TCP addresses.
func ClientAddressDialer(defaultPort string) func(string, time.Duration) (net.Conn, error) {
	return func(addr string, timeout time.Duration) (net.Conn, error) {
		parsedAddr, err := ParseAddressString(
			addr, defaultPort, net.ResolveTCPAddr,
		)
		if err != nil {
			return nil, err
		}

		return net.DialTimeout(
			parsedAddr.Network(), parsedAddr.String(), timeout,
		)
	}
}
