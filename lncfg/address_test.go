// +build !rpctest

package lncfg

import "testing"

// addressTest defines a test vector for an address that contains the non-
// normalized input and the expected normalized output.
type addressTest struct {
	address         string
	expectedNetwork string
	expectedAddress string
	isLoopback      bool
	isUnix          bool
}

var (
	defaultTestPort    = "1234"
	addressTestVectors = []addressTest{
		{"tcp://127.0.0.1:9735", "tcp", "127.0.0.1:9735", true, false},
		{"tcp:127.0.0.1:9735", "tcp", "127.0.0.1:9735", true, false},
		{"127.0.0.1:9735", "tcp", "127.0.0.1:9735", true, false},
		{":9735", "tcp", ":9735", false, false},
		{"", "tcp", ":1234", false, false},
		{":", "tcp", ":1234", false, false},
		{"tcp4://127.0.0.1:9735", "tcp", "127.0.0.1:9735", true, false},
		{"tcp4:127.0.0.1:9735", "tcp", "127.0.0.1:9735", true, false},
		{"127.0.0.1", "tcp", "127.0.0.1:1234", true, false},
		{"[::1]", "tcp", "[::1]:1234", true, false},
		{"::1", "tcp", "[::1]:1234", true, false},
		{"tcp6://::1", "tcp", "[::1]:1234", true, false},
		{"tcp6:::1", "tcp", "[::1]:1234", true, false},
		{"localhost:9735", "tcp", "127.0.0.1:9735", true, false},
		{"localhost", "tcp", "127.0.0.1:1234", true, false},
		{"unix:///tmp/lnd.sock", "unix", "/tmp/lnd.sock", false, true},
		{"unix:/tmp/lnd.sock", "unix", "/tmp/lnd.sock", false, true},
	}
	invalidTestVectors = []string{
		"some string",
		"://",
		"12.12.12",
		"123",
	}
)

// TestAddresses ensures that all supported address formats can be parsed and
// normalized correctly.
func TestAddresses(t *testing.T) {
	// First, test all correct addresses.
	for _, testVector := range addressTestVectors {
		addr := []string{testVector.address}
		normalized, err := NormalizeAddresses(addr, defaultTestPort)
		if err != nil {
			t.Fatalf("unable to normalize address %s: %v",
				testVector.address, err)
		}
		netAddr := normalized[0]
		if err != nil {
			t.Fatalf("unable to split normalized address: %v", err)
		}
		if netAddr.Network() != testVector.expectedNetwork ||
			netAddr.String() != testVector.expectedAddress {
			t.Fatalf(
				"mismatched address: expected %s://%s, got "+
					"%s://%s",
				testVector.expectedNetwork,
				testVector.expectedAddress,
				netAddr.Network(), netAddr.String(),
			)
		}
		isAddrLoopback := IsLoopback(normalized[0])
		if testVector.isLoopback != isAddrLoopback {
			t.Fatalf(
				"mismatched loopback detection: expected "+
					"%v, got %v for addr %s",
				testVector.isLoopback, isAddrLoopback,
				testVector.address,
			)
		}
		isAddrUnix := IsUnix(normalized[0])
		if testVector.isUnix != isAddrUnix {
			t.Fatalf(
				"mismatched unix detection: expected "+
					"%v, got %v for addr %s",
				testVector.isUnix, isAddrUnix,
				testVector.address,
			)
		}
	}

	// Finally, test invalid inputs to see if they are handled correctly.
	for _, testVector := range invalidTestVectors {
		addr := []string{testVector}
		_, err := NormalizeAddresses(addr, defaultTestPort)
		if err == nil {
			t.Fatalf("expected error when parsing %v", testVector)
		}
	}
}
