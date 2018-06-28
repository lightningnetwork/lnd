package lncfg

import (
	"time"
	"net"
	"fmt"
	"crypto/tls"
	"strings"
)

var (
	loopBackAddrs = []string{"localhost", "127.0.0.1", "[::1]"}
)

// NormalizeAddresses returns a new slice with all the passed addresses
// normalized with the given default port and all duplicates removed.
func NormalizeAddresses(addrs []string,
	defaultPort string) ([]net.Addr, error) {
	result := make([]net.Addr, 0, len(addrs))
	seen := map[string]struct{}{}
	for _, strAddr := range addrs {
		addr, err := ParseAddressString(strAddr, defaultPort)
		if err != nil {
			return nil, err
		}

		if _, ok := seen[addr.String()]; !ok {
			result = append(result, addr)
			seen[addr.String()] = struct{}{}
		}
	}
	return result, nil
}

// EnforceSafeAuthentication enforces "safe" authentication taking into account
// the interfaces that the RPC servers are listening on, and if macaroons are
// activated or not. To protect users from using dangerous config combinations,
// we'll prevent disabling authentication if the sever is listening on a public
// interface.
func EnforceSafeAuthentication(addrs []net.Addr, macaroonsActive bool) error {
	// We'll now examine all addresses that this RPC server is listening
	// on. If it's a localhost address, we'll skip it, otherwise, we'll
	// return an error if macaroons are inactive.
	for _, addr := range addrs {
		if IsLoopback(addr) || IsUnix(addr) {
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

// ListenOnAddress creates a listener that listens on the given
// address.
func ListenOnAddress(addr net.Addr) (net.Listener, error) {
	return net.Listen(addr.Network(), addr.String())
}

// TlsListenOnAddress creates a TLS listener that listens on the given
// address.
func TlsListenOnAddress(addr net.Addr,
	config *tls.Config) (net.Listener, error) {
	return tls.Listen(addr.Network(), addr.String(), config)
}

// IsLoopback returns true if an address describes a loopback interface.
func IsLoopback(addr net.Addr) bool {
	for _, loopback := range loopBackAddrs {
		if strings.Contains(addr.String(), loopback) {
			return true
		}
	}

	return false
}

// isUnix returns true if an address describes an Unix socket address.
func IsUnix(addr net.Addr) bool {
	return strings.HasPrefix(addr.Network(), "unix")
}

// ParseAddressString converts an address in string format to a net.Addr that is
// compatible with lnd. UDP is not supported because lnd needs reliable
// connections.
func ParseAddressString(strAddress string,
	defaultPort string) (net.Addr, error) {
	var parsedNetwork, parsedAddr string

	// Addresses can either be in network://address:port format or only
	// address:port. We want to support both.
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
		return net.ResolveTCPAddr(parsedNetwork,
			verifyPort(parsedAddr, defaultPort))
	case "ip", "ip4", "ip6", "udp", "udp4", "udp6", "unixgram":
		return nil, fmt.Errorf("only TCP or unix socket "+
			"addresses are supported: %s", parsedAddr)
	default:
		// There was no network specified, just try to parse as host
		// and port.
		return net.ResolveTCPAddr(
			"tcp", verifyPort(strAddress, defaultPort),
		)
	}
}

// verifyPort makes sure that an address string has both a host and a port.
// If there is no port found, the default port is appended.
func verifyPort(strAddress string, defaultPort string) string {
	host, port, err := net.SplitHostPort(strAddress)
	if err != nil {
		// If we already have an IPv6 address with brackets, don't use
		// the JoinHostPort function, since it will always add a pair
		// of brackets too.
		if strings.HasPrefix(strAddress, "[") {
			strAddress = strAddress + ":" + defaultPort
		} else {
			strAddress = net.JoinHostPort(strAddress, defaultPort)
		}
	} else if host == "" && port == "" {
		// The string ':' is parsed as valid empty host and empty port.
		// But in that case, we want the default port to be applied too.
		strAddress = ":" + defaultPort
	}

	return strAddress
}

// ClientAddressDialer creates a gRPC dialer that can also dial unix socket
// addresses instead of just TCP addresses.
func ClientAddressDialer(defaultPort string) func(string,
	time.Duration) (net.Conn, error) {
	return func(addr string, timeout time.Duration) (net.Conn, error) {
		parsedAddr, err := ParseAddressString(addr, defaultPort)
		if err != nil {
			return nil, err
		}
		return net.DialTimeout(parsedAddr.Network(),
			parsedAddr.String(), timeout)
	}
}
