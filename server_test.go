package lnd

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// mockTorNet is a mock implementation of tor.Net for testing.
type mockTorNet struct {
	dialConn net.Conn
	dialErr  error

	lookupHostResults []string
	lookupHostErr     error

	lookupSRVCname   string
	lookupSRVResults []*net.SRV
	lookupSRVErr     error

	resolveTCPAddrResult *net.TCPAddr
	resolveTCPAddrErr    error
}

// Dial implements the tor.Net Dial method for testing.
func (m *mockTorNet) Dial(network, address string,
	timeout time.Duration) (net.Conn, error) {

	return m.dialConn, m.dialErr
}

// LookupHost implements the tor.Net LookupHost method for testing.
func (m *mockTorNet) LookupHost(host string) ([]string, error) {
	return m.lookupHostResults, m.lookupHostErr
}

// LookupSRV implements the tor.Net LookupSRV method for testing.
func (m *mockTorNet) LookupSRV(service, proto, name string,
	timeout time.Duration) (string, []*net.SRV, error) {

	return m.lookupSRVCname, m.lookupSRVResults, m.lookupSRVErr
}

// ResolveTCPAddr implements the tor.Net ResolveTCPAddr method for testing.
func (m *mockTorNet) ResolveTCPAddr(network,
	address string) (*net.TCPAddr, error) {

	return m.resolveTCPAddrResult, m.resolveTCPAddrErr
}

// TestShouldPeerBootstrap tests that we properly skip network bootstrap for
// the developer networks, and also if bootstrapping is explicitly disabled.
func TestShouldPeerBootstrap(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		cfg            *Config
		shouldBoostrap bool
	}{
		// Simnet active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					SimNet: true,
				},
			},
		},

		// Regtest active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					RegTest: true,
				},
			},
		},

		// Signet active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					SigNet: true,
				},
			},
		},

		// Mainnet active, but bootstrap disabled, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					MainNet: true,
				},
				NoNetBootstrap: true,
			},
		},

		// Mainnet active, should bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					MainNet: true,
				},
			},
			shouldBoostrap: true,
		},

		// Testnet active, should bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					TestNet3: true,
				},
			},
			shouldBoostrap: true,
		},
	}
	for i, testCase := range testCases {
		bootstrapped := shouldPeerBootstrap(testCase.cfg)
		if bootstrapped != testCase.shouldBoostrap {
			t.Fatalf("#%v: expected bootstrap=%v, got bootstrap=%v",
				i, testCase.shouldBoostrap, bootstrapped)
		}
	}
}

// TestParseAddress verifies the behavior of parseAddr for a variety of address
// formats and input scenarios. It checks correct parsing of onion addresses, IP
// addresses, with or without ports, as well as appropriate
// error handling for invalid inputs.
func TestParseAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		address   string
		netCfg    tor.Net
		expected  net.Addr
		expectErr bool
		errMsg    string
	}{
		{
			name:    "OnionAddress WithExplicitPort ReturnsAddr",
			address: "3g2upl4pq6kufc4m.onion:9735",
			netCfg:  &mockTorNet{},
			expected: &tor.OnionAddr{
				OnionService: "3g2upl4pq6kufc4m.onion",
				Port:         9735,
			},
			expectErr: false,
		},
		{
			name:    "OnionAddress WithoutPort ReturnsAddrWithPort",
			address: "3g2upl4pq6kufc4m.onion",
			netCfg:  &mockTorNet{},
			expected: &tor.OnionAddr{
				OnionService: "3g2upl4pq6kufc4m.onion",
				Port:         defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "LoopbackAddress WithExplicitPort ReturnsAddr",
			address: "127.0.0.1:8080",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("127.0.0.1"),
					Port: 8080,
				},
			},
			expected: &net.TCPAddr{
				IP:   net.ParseIP("127.0.0.1"),
				Port: 8080,
			},
			expectErr: false,
		},
		{
			name:    "LoopbackAddress WithoutPort ReturnsAddr",
			address: "127.0.0.1",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("127.0.0.1"),
					Port: defaultPeerPort,
				},
			},
			expected: &net.TCPAddr{
				IP:   net.ParseIP("127.0.0.1"),
				Port: defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "IPv4Address WithExplicitPort ReturnsTCPAddr",
			address: "192.168.1.1:9735",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("192.168.1.1"),
					Port: 9735,
				},
			},
			expected: &net.TCPAddr{
				IP:   net.ParseIP("192.168.1.1"),
				Port: 9735,
			},
			expectErr: false,
		},
		{
			name:    "IPv4Address WithoutPort ReturnsAddrWithPort",
			address: "192.168.1.1",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("192.168.1.1"),
					Port: defaultPeerPort,
				},
			},
			expected: &net.TCPAddr{
				IP:   net.ParseIP("192.168.1.1"),
				Port: defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "IPv6Address WithExplicitPort ReturnsTCPAddr",
			address: "[2001:db8::1]:9735",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("2001:db8::1"),
					Port: 9735,
				},
			},
			expected: &net.TCPAddr{
				IP:   net.ParseIP("2001:db8::1"),
				Port: 9735,
			},
			expectErr: false,
		},
		{
			name:    "IPv6Address WithoutPort ReturnsAddrWithPort",
			address: "2001:db8::1",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("2001:db8::1"),
					Port: defaultPeerPort,
				},
			},
			expected: &net.TCPAddr{
				IP:   net.ParseIP("2001:db8::1"),
				Port: defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "IPAddress TCPResolutionFailure ReturnsError",
			address: "192.168.1.1:9735",
			netCfg: &mockTorNet{
				resolveTCPAddrErr: errors.New("resolve error"),
			},
			expectErr: true,
			errMsg:    "resolve error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addr, err := parseAddr(test.address, test.netCfg)
			if test.expectErr {
				require.ErrorContains(t, err, test.errMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, addr)
		})
	}
}
