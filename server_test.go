package lnd

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
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

func TestParseAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
		netCfg  tor.Net
		want    net.Addr
		wantErr bool
		errMsg  string
	}{
		{
			//nolint:ll
			name:    "OnionAddress_WithExplicitPort_ReturnsOnionAddr",
			address: "3g2upl4pq6kufc4m.onion:9735",
			netCfg:  &mockTorNet{},
			want: &tor.OnionAddr{
				OnionService: "3g2upl4pq6kufc4m.onion",
				Port:         9735,
			},
			wantErr: false,
		},
		{
			//nolint:ll
			name:    "OnionAddress_WithoutPort_ReturnsOnionAddrWithDefaultPort",
			address: "3g2upl4pq6kufc4m.onion",
			netCfg:  &mockTorNet{},
			want: &tor.OnionAddr{
				OnionService: "3g2upl4pq6kufc4m.onion",
				Port:         defaultPeerPort,
			},
			wantErr: false,
		},
		{
			//nolint:ll
			name:    "LoopbackAddress_WithExplicitPort_ReturnsTCPAddr",
			address: "127.0.0.1:8080",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("127.0.0.1"),
					Port: 8080,
				},
			},
			want: &net.TCPAddr{
				IP:   net.ParseIP("127.0.0.1"),
				Port: 8080,
			},
			wantErr: false,
		},
		{
			//nolint:ll
			name:    "LoopbackAddress_WithoutPort_ReturnsTCPAddrWithDefaultPort",
			address: "127.0.0.1",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("127.0.0.1"),
					Port: defaultPeerPort,
				},
			},
			want: &net.TCPAddr{
				IP:   net.ParseIP("127.0.0.1"),
				Port: defaultPeerPort,
			},
			wantErr: false,
		},
		{
			name:    "IPv4Address_WithExplicitPort_ReturnsTCPAddr",
			address: "192.168.1.1:9735",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("192.168.1.1"),
					Port: 9735,
				},
			},
			want: &net.TCPAddr{
				IP:   net.ParseIP("192.168.1.1"),
				Port: 9735,
			},
			wantErr: false,
		},
		{
			//nolint:ll
			name:    "IPv4Address_WithoutPort_ReturnsTCPAddrWithDefaultPort",
			address: "192.168.1.1",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("192.168.1.1"),
					Port: defaultPeerPort,
				},
			},
			want: &net.TCPAddr{
				IP:   net.ParseIP("192.168.1.1"),
				Port: defaultPeerPort,
			},
			wantErr: false,
		},
		{
			name:    "IPv6Address_WithExplicitPort_ReturnsTCPAddr",
			address: "[2001:db8::1]:9735",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("2001:db8::1"),
					Port: 9735,
				},
			},
			want: &net.TCPAddr{
				IP:   net.ParseIP("2001:db8::1"),
				Port: 9735,
			},
			wantErr: false,
		},
		{
			//nolint:ll
			name:    "IPv6Address_WithoutPort_ReturnsTCPAddrWithDefaultPort",
			address: "2001:db8::1",
			netCfg: &mockTorNet{
				resolveTCPAddrResult: &net.TCPAddr{
					IP:   net.ParseIP("2001:db8::1"),
					Port: defaultPeerPort,
				},
			},
			want: &net.TCPAddr{
				IP:   net.ParseIP("2001:db8::1"),
				Port: defaultPeerPort,
			},
			wantErr: false,
		},
		{
			//nolint:ll
			name:    "DNSHostname_WithExplicitPort_ReturnsDNSHostnameAddress",
			address: "example.com:8080",
			netCfg: &mockTorNet{
				lookupHostResults: []string{"192.0.2.1"},
			},
			want: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     8080,
			},
			wantErr: false,
		},
		{
			//nolint:ll
			name:    "DNSHostname_WithoutPort_ReturnsDNSHostnameAddressWithDefaultPort",
			address: "example.com",
			netCfg: &mockTorNet{
				lookupHostResults: []string{"192.0.2.1"},
			},
			want: &lnwire.DNSHostnameAddress{
				Hostname: "example.com",
				Port:     defaultPeerPort,
			},
			wantErr: false,
		},
		{
			name:    "Hostname_DNSLookupFailure_ReturnsError",
			address: "nonexistent-domain.invalid",
			netCfg: &mockTorNet{
				lookupHostErr: errors.New("lookup failed"),
			},
			wantErr: true,
			errMsg:  "invalid address: nonexistent-domain.invalid",
		},
		{
			name:    "DNSHostname_InvalidFormat_ReturnsError",
			address: "invalid..hostname.com",
			netCfg: &mockTorNet{
				lookupHostErr: errors.New("invalid hostname " +
					"format"),
			},
			wantErr: true,
			errMsg:  "invalid address: invalid..hostname.com",
		},
		{
			//nolint:ll
			name:    "Address_NonNumericPort_ReturnsPortParsingError",
			address: "example.com:invalid",
			netCfg:  &mockTorNet{},
			wantErr: true,
			errMsg:  "strconv.Atoi",
		},
		{
			//nolint:ll
			name:    "Address_EmptyString_ReturnsInvalidAddressError",
			address: "",
			netCfg: &mockTorNet{
				lookupHostErr: errors.New("empty address"),
			},
			wantErr: true,
			errMsg:  "invalid address",
		},
		{
			name:    "IPAddress_TCPResolutionFailure_ReturnsError",
			address: "192.168.1.1:9735",
			netCfg: &mockTorNet{
				resolveTCPAddrErr: errors.New("resolve error"),
			},
			wantErr: true,
			errMsg:  "resolve error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addr, err := parseAddr(test.address, test.netCfg)

			if err == nil && test.wantErr {
				t.Fatal("expected an error, but got none")
			}
			if err != nil && !test.wantErr {
				t.Fatalf("unexpected error, got: %v", err)
			}

			if err != nil && test.wantErr &&
				!strings.Contains(err.Error(), test.errMsg) {

				t.Fatalf("expected error message %s to be in "+
					"%s", test.errMsg, err.Error())
			}

			if err == nil && !reflect.DeepEqual(addr, test.want) {
				t.Errorf("parseAddr() = %v, "+
					"want %v", addr, test.want)
			}
		})
	}
}
