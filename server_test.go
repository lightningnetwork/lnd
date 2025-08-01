package lnd

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestParseAddress verifies the behavior of parseAddr for a variety of address
// formats and input scenarios.
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
			name:    "OnionAddressWithExplicitPortReturnsAddr",
			address: "3g2upl4pq6kufc4m.onion:9735",
			netCfg:  new(MockTorNet),
			expected: &tor.OnionAddr{
				OnionService: "3g2upl4pq6kufc4m.onion",
				Port:         9735,
			},
			expectErr: false,
		},
		{
			name:    "OnionAddressWithoutPortReturnsAddrWithPort",
			address: "3g2upl4pq6kufc4m.onion",
			netCfg:  new(MockTorNet),
			expected: &tor.OnionAddr{
				OnionService: "3g2upl4pq6kufc4m.onion",
				Port:         defaultPeerPort,
			},
			expectErr: false,
		},
		{
			name:    "LoopbackAddressWithExplicitPortReturnsAddr",
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
			name:    "LoopbackAddressWithoutPortReturnsAddr",
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
			name:    "IPv4AddressWithExplicitPortReturnsTCPAddr",
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
			name:    "IPv4AddressWithoutPortReturnsAddrWithPort",
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
			name:    "IPv6AddressWithExplicitPortReturnsTCPAddr",
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
			name:    "IPv6AddressWithoutPortReturnsAddrWithPort",
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
			name:    "IPAddressTCPResolutionFailureReturnsError",
			address: "10.1.1.1:9735",
			netCfg: func() tor.Net {
				mockNet := new(MockTorNet)
				mockExpectation := mockNet.On(
					"ResolveTCPAddr", "tcp",
					"10.1.1.1:9735",
				)
				mockExpectation.Return(
					&net.TCPAddr{},
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

// MockTorNet is a mock implementation of tor.Net.
type MockTorNet struct {
	mock.Mock
}

// Dial implements the tor.Net Dial method for testing.
func (m *MockTorNet) Dial(network, address string,
	timeout time.Duration) (net.Conn, error) {

	args := m.Called(network, address, timeout)
	arg0, ok := args.Get(0).(net.Conn)
	if !ok {
		return nil, errors.New("type assertion failed for arg0")
	}

	return arg0, args.Error(1)
}

// LookupHost implements the tor.Net LookupHost method for testing.
func (m *MockTorNet) LookupHost(host string) ([]string, error) {
	args := m.Called(host)
	arg0, ok := args.Get(0).([]string)
	if !ok {
		return nil, errors.New("type assertion failed for arg0")
	}

	return arg0, args.Error(1)
}

// LookupSRV implements the tor.Net LookupSRV method for testing.
func (m *MockTorNet) LookupSRV(service, proto, name string,
	timeout time.Duration) (string, []*net.SRV, error) {

	args := m.Called(service, proto, name, timeout)

	arg0, ok := args.Get(0).(string)
	if !ok {
		return "", nil, errors.New("type assertion failed for arg0")
	}

	arg1, ok := args.Get(1).([]*net.SRV)
	if !ok {
		return "", nil, errors.New("type assertion failed for arg1")
	}

	return arg0, arg1, args.Error(2)
}

// ResolveTCPAddr implements the tor.Net ResolveTCPAddr method for testing.
func (m *MockTorNet) ResolveTCPAddr(network,
	address string) (*net.TCPAddr, error) {

	args := m.Called(network, address)
	arg0, ok := args.Get(0).(*net.TCPAddr)
	if !ok {
		return nil, errors.New("type assertion failed for arg0")
	}

	return arg0, args.Error(1)
}
