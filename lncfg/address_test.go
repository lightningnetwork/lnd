// +build !rpctest

package lncfg

import (
	"net"
	"testing"
)

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
		{"123", "tcp", "127.0.0.1:123", true, false},
		{
			"4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnzjpbi7utijcltosqemad.onion",
			"tcp",
			"4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnzjpbi7utijcltosqemad.onion:1234",
			false,
			false,
		},
		{
			"4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnzjpbi7utijcltosqemad.onion:9735",
			"tcp",
			"4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnzjpbi7utijcltosqemad.onion:9735",
			false,
			false,
		},
		{
			"3g2upl4pq6kufc4m.onion",
			"tcp",
			"3g2upl4pq6kufc4m.onion:1234",
			false,
			false,
		},
		{
			"3g2upl4pq6kufc4m.onion:9735",
			"tcp",
			"3g2upl4pq6kufc4m.onion:9735",
			false,
			false,
		},
	}
	invalidTestVectors = []string{
		"some string",
		"://",
		"12.12.12.12.12",
	}
)

// TestAddresses ensures that all supported address formats can be parsed and
// normalized correctly.
func TestAddresses(t *testing.T) {
	// First, test all correct addresses.
	for i, testVector := range addressTestVectors {
		addr := []string{testVector.address}
		normalized, err := NormalizeAddresses(
			addr, defaultTestPort, net.ResolveTCPAddr,
		)
		if err != nil {
			t.Fatalf("#%v: unable to normalize address %s: %v",
				i, testVector.address, err)
		}
		netAddr := normalized[0]
		if err != nil {
			t.Fatalf("#%v: unable to split normalized address: %v", i, err)
		}
		if netAddr.Network() != testVector.expectedNetwork ||
			netAddr.String() != testVector.expectedAddress {
			t.Fatalf("#%v: mismatched address: expected %s://%s, "+
				"got %s://%s",
				i, testVector.expectedNetwork,
				testVector.expectedAddress,
				netAddr.Network(), netAddr.String(),
			)
		}
		isAddrLoopback := IsLoopback(normalized[0].String())
		if testVector.isLoopback != isAddrLoopback {
			t.Fatalf("#%v: mismatched loopback detection: expected "+
				"%v, got %v for addr %s",
				i, testVector.isLoopback, isAddrLoopback,
				testVector.address,
			)
		}
		isAddrUnix := IsUnix(normalized[0])
		if testVector.isUnix != isAddrUnix {
			t.Fatalf("#%v: mismatched unix detection: expected "+
				"%v, got %v for addr %s",
				i, testVector.isUnix, isAddrUnix,
				testVector.address,
			)
		}
	}

	// Finally, test invalid inputs to see if they are handled correctly.
	for _, testVector := range invalidTestVectors {
		addr := []string{testVector}
		_, err := NormalizeAddresses(
			addr, defaultTestPort, net.ResolveTCPAddr,
		)
		if err == nil {

			t.Fatalf("expected error when parsing %v", testVector)
		}
	}
}
