package lnd

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockTorNet is a mock implementation of tor.Net.
type MockTorNet struct {
	mock.Mock
}

// Dial implements the tor.Net Dial method for testing.
func (m *MockTorNet) Dial(network, address string,
	timeout time.Duration) (net.Conn, error) {

	args := m.Called(network, address, timeout)
	if conn := args.Get(0); conn != nil {
		return conn.(net.Conn), args.Error(1)
	}
	return nil, args.Error(1)
}

// LookupHost implements the tor.Net LookupHost method for testing.
func (m *MockTorNet) LookupHost(host string) ([]string, error) {
	args := m.Called(host)
	if results := args.Get(0); results != nil {
		return results.([]string), args.Error(1)
	}
	return nil, args.Error(1)
}

// LookupSRV implements the tor.Net LookupSRV method for testing.
func (m *MockTorNet) LookupSRV(service, proto, name string,
	timeout time.Duration) (string, []*net.SRV, error) {

	args := m.Called(service, proto, name, timeout)

	var cname string
	if cnameArg := args.Get(0); cnameArg != nil {
		cname = cnameArg.(string)
	}

	var results []*net.SRV
	if resultsArg := args.Get(1); resultsArg != nil {
		results = resultsArg.([]*net.SRV)
	}

	return cname, results, args.Error(2)
}

// ResolveTCPAddr implements the tor.Net ResolveTCPAddr method for testing.
func (m *MockTorNet) ResolveTCPAddr(network,
	address string) (*net.TCPAddr, error) {

	args := m.Called(network, address)
	if addr := args.Get(0); addr != nil {
		return addr.(*net.TCPAddr), args.Error(1)
	}
	return nil, args.Error(1)
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
// addresses, and DNS hostnames, with or without ports, as well as appropriate
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
			netCfg:  new(MockTorNet),
			expected: &tor.OnionAddr{
				OnionService: "3g2upl4pq6kufc4m.onion",
				Port:         9735,
			},
			expectErr: false,
		},
		{
			name:    "OnionAddress WithoutPort ReturnsAddrWithPort",
			address: "3g2upl4pq6kufc4m.onion",
			netCfg:  new(MockTorNet),
			expected: &tor.OnionAddr{
				OnionService: "3g2upl4pq6kufc4m.onion",
				Port:         defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "LoopbackAddress WithExplicitPort ReturnsAddr",
			address: "127.0.0.1:8080",
			netCfg: func() tor.Net {
				mockNet := new(MockTorNet)
				mockExpectation := mockNet.On(
					"ResolveTCPAddr", "tcp",
					"127.0.0.1:8080",
				)
				mockExpectation.Return(
					&net.TCPAddr{
						IP:   net.ParseIP("127.0.0.1"),
						Port: 8080,
					},
					nil,
				)
				return mockNet
			}(),
			expected: &net.TCPAddr{
				IP:   net.ParseIP("127.0.0.1"),
				Port: 8080,
			},
			expectErr: false,
		},
		{
			name:    "LoopbackAddress WithoutPort ReturnsAddr",
			address: "127.0.0.1",
			netCfg: func() tor.Net {
				mockNet := new(MockTorNet)
				mockExpectation := mockNet.On(
					"ResolveTCPAddr", "tcp",
					"127.0.0.1:9735",
				)
				mockExpectation.Return(
					&net.TCPAddr{
						IP:   net.ParseIP("127.0.0.1"),
						Port: defaultPeerPort,
					},
					nil,
				)
				return mockNet
			}(),
			expected: &net.TCPAddr{
				IP:   net.ParseIP("127.0.0.1"),
				Port: defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "IPv4Address WithExplicitPort ReturnsTCPAddr",
			address: "10.1.1.1:9735",
			netCfg: func() tor.Net {
				mockNet := new(MockTorNet)
				mockExpectation := mockNet.On(
					"ResolveTCPAddr", "tcp",
					"10.1.1.1:9735",
				)
				mockExpectation.Return(
					&net.TCPAddr{
						IP:   net.ParseIP("10.1.1.1"),
						Port: 9735,
					},
					nil,
				)
				return mockNet
			}(),
			expected: &net.TCPAddr{
				IP:   net.ParseIP("10.1.1.1"),
				Port: 9735,
			},
			expectErr: false,
		},
		{
			name:    "IPv4Address WithoutPort ReturnsAddrWithPort",
			address: "10.1.1.1",
			netCfg: func() tor.Net {
				mockNet := new(MockTorNet)
				mockExpectation := mockNet.On(
					"ResolveTCPAddr", "tcp",
					"10.1.1.1:9735",
				)
				mockExpectation.Return(
					&net.TCPAddr{
						IP:   net.ParseIP("10.1.1.1"),
						Port: defaultPeerPort,
					},
					nil,
				)
				return mockNet
			}(),
			expected: &net.TCPAddr{
				IP:   net.ParseIP("10.1.1.1"),
				Port: defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "IPv6Address WithExplicitPort ReturnsTCPAddr",
			address: "[2001::]:9735",
			netCfg: func() tor.Net {
				mockNet := new(MockTorNet)
				mockExpectation := mockNet.On(
					"ResolveTCPAddr", "tcp",
					"[2001::]:9735",
				)
				mockExpectation.Return(
					&net.TCPAddr{
						IP:   net.ParseIP("2001::"),
						Port: 9735,
					},
					nil,
				)
				return mockNet
			}(),
			expected: &net.TCPAddr{
				IP:   net.ParseIP("2001::"),
				Port: 9735,
			},
			expectErr: false,
		},
		{
			name:    "IPv6Address WithoutPort ReturnsAddrWithPort",
			address: "2001::",
			netCfg: func() tor.Net {
				mockNet := new(MockTorNet)
				mockExpectation := mockNet.On(
					"ResolveTCPAddr", "tcp",
					"[2001::]:9735",
				)
				mockExpectation.Return(
					&net.TCPAddr{
						IP:   net.ParseIP("2001::"),
						Port: defaultPeerPort,
					},
					nil,
				)
				return mockNet
			}(),
			expected: &net.TCPAddr{
				IP:   net.ParseIP("2001::"),
				Port: defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "IPAddress TCPResolutionFailure ReturnsError",
			address: "10.1.1.1:9735",
			netCfg: func() tor.Net {
				mockNet := new(MockTorNet)
				mockExpectation := mockNet.On(
					"ResolveTCPAddr", "tcp",
					"10.1.1.1:9735",
				)
				mockExpectation.Return(
					nil,
					errors.New("resolve error"),
				)
				return mockNet
			}(),
			expectErr: true,
			errMsg:    "resolve error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			addr, err := parseAddr(test.address, test.netCfg)

			if test.expectErr {
				require.ErrorContains(t, err, test.errMsg)
				return
			}

			require.NoError(t, err)
			require.Equal(t, test.expected, addr)

			// Verify that all expected mock method calls were made.
			if mockNet, ok := test.netCfg.(*MockTorNet); ok {
				mockNet.AssertExpectations(t)
			}
		})
	}
}
