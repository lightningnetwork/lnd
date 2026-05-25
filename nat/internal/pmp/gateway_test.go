package pmp

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseLinuxIPRouteShow locks in the iproute2 default-route parser.
func TestParseLinuxIPRouteShow(t *testing.T) {
	t.Parallel()

	out := []byte("default via 192.168.178.1 dev wlp3s0 metric 303\n" +
		"192.168.178.0/24 dev wlp3s0 proto kernel scope link " +
		"src 192.168.178.76 metric 303\n")
	ip, err := parseLinuxIPRouteShow(out)
	require.NoError(t, err)
	require.True(t, ip.Equal(net.ParseIP("192.168.178.1")))
}

// TestParseLinuxIPRouteGet exercises `ip route get` output, which has a
// different field layout than `ip route show`.
func TestParseLinuxIPRouteGet(t *testing.T) {
	t.Parallel()

	out := []byte("8.8.8.8 via 10.0.1.1 dev eth0 src 10.0.1.36 uid 2000\n")
	ip, err := parseLinuxIPRouteGet(out)
	require.NoError(t, err)
	require.True(t, ip.Equal(net.ParseIP("10.0.1.1")))
}

// TestParseLinuxRoute exercises the legacy `route -n` table.
func TestParseLinuxRoute(t *testing.T) {
	t.Parallel()

	out := []byte("Kernel IP routing table\n" +
		"Destination     Gateway         Genmask         Flags ...\n" +
		"0.0.0.0         192.168.1.1     0.0.0.0         UG    ...\n")
	ip, err := parseLinuxRoute(out)
	require.NoError(t, err)
	require.True(t, ip.Equal(net.ParseIP("192.168.1.1")))
}

// TestParseDarwinRouteGet exercises the `route -n get` colon-separated
// format used on macOS / BSD.
func TestParseDarwinRouteGet(t *testing.T) {
	t.Parallel()

	out := []byte("   route to: default\n" +
		"destination: default\n" +
		"       mask: default\n" +
		"    gateway: 192.168.1.1\n")
	ip, err := parseDarwinRouteGet(out)
	require.NoError(t, err)
	require.True(t, ip.Equal(net.ParseIP("192.168.1.1")))
}

// TestParseBSDSolarisNetstat exercises the FreeBSD/Solaris `netstat -rn`
// layout. The parser must skip the IPv6 routing block.
func TestParseBSDSolarisNetstat(t *testing.T) {
	t.Parallel()

	out := []byte("Routing tables\n\n" +
		"Internet:\n" +
		"Destination        Gateway            Flags      Netif Expire\n" +
		"default            10.88.88.2         UGS         em0\n" +
		"10.88.88.0/24      link#1             U           em0\n\n" +
		"Internet6:\n" +
		"::/96              ::1                UGRS        lo0\n")
	ip, err := parseBSDSolarisNetstat(out)
	require.NoError(t, err)
	require.True(t, ip.Equal(net.ParseIP("10.88.88.2")))
}

// TestParseWindowsRoutePrint exercises the Windows `route print` layout.
func TestParseWindowsRoutePrint(t *testing.T) {
	t.Parallel()

	out := []byte("=========================\n" +
		"Active Routes:\n" +
		"Network Destination        Netmask          Gateway       Interface  Metric\n" +
		"          0.0.0.0          0.0.0.0      192.168.1.1    192.168.1.100     20\n" +
		"=========================\n")
	ip, err := parseWindowsRoutePrint(out)
	require.NoError(t, err)
	require.True(t, ip.Equal(net.ParseIP("192.168.1.1")))
}

// TestParseNoGateway verifies each parser surfaces errNoGateway rather
// than returning a zero IP when the input has no default route.
func TestParseNoGateway(t *testing.T) {
	t.Parallel()

	for name, fn := range map[string]func([]byte) (net.IP, error){
		"iprouteshow": parseLinuxIPRouteShow,
		"iprouteget":  parseLinuxIPRouteGet,
		"route":       parseLinuxRoute,
		"darwin":      parseDarwinRouteGet,
		"bsd":         parseBSDSolarisNetstat,
		"windows":     parseWindowsRoutePrint,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fn([]byte("garbage output with no default route\n"))
			require.ErrorIs(t, err, errNoGateway)
		})
	}
}
