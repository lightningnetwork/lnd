package tor

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	testOnion  = "ld47qlr6h2b7hrrf.onion"
	testFakeIP = "fd87:d87e:eb43:58f9:f82e:3e3e:83f3:c625"
)

// TestFakeIPToOnionHost tests that a fake tcp6 address can be converted back
// into its original .onion host address successfully.
func TestFakeIPToOnionHost(t *testing.T) {
	tcpAddr, err := net.ResolveTCPAddr(
		"tcp6", fmt.Sprintf("[%s]:8333", testFakeIP),
	)
	require.NoError(t, err)
	require.True(t, IsOnionFakeIP(tcpAddr))

	onionHost, err := FakeIPToOnionHost(tcpAddr)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%s:8333", testOnion), onionHost.String())
}
