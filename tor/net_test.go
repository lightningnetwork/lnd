package tor

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProxyNetSkipProxyLookups checks that, with SkipProxyForClearNetTargets
// set, ProxyNet resolves clearnet hosts directly rather than through Tor, the
// same way Dial connects to them (lnd#11584), while onion hosts still go
// through Tor.
func TestProxyNetSkipProxyLookups(t *testing.T) {
	t.Parallel()

	// Nothing listens on this address, so any lookup that goes through
	// the SOCKS proxy fails.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadSOCKS := l.Addr().String()
	require.NoError(t, l.Close())

	const (
		clearHost = "localhost"
		onionHost = "duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twag" +
			"swzczad.onion"
	)
	require.True(t, IsOnionHost(onionHost))

	skipping := &ProxyNet{
		SOCKS: deadSOCKS, SkipProxyForClearNetTargets: true,
	}
	proxied := &ProxyNet{SOCKS: deadSOCKS}

	// Clearnet hosts are resolved directly when skipping the proxy.
	addrs, err := skipping.LookupHost(clearHost)
	require.NoError(t, err)
	require.NotEmpty(t, addrs)

	tcpAddr, err := skipping.ResolveTCPAddr("tcp", clearHost+":8333")
	require.NoError(t, err)
	require.Equal(t, 8333, tcpAddr.Port)

	// Without the flag, the same lookups go through the proxy.
	_, err = proxied.LookupHost(clearHost)
	require.Error(t, err)
	_, err = proxied.ResolveTCPAddr("tcp", clearHost+":8333")
	require.Error(t, err)

	// Onion hosts always go through the proxy.
	_, err = skipping.LookupHost(onionHost)
	require.Error(t, err)
	_, err = skipping.ResolveTCPAddr("tcp", onionHost+":8333")
	require.Error(t, err)
}
