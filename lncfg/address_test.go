package lncfg

import (
	"bytes"
	"encoding/hex"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
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
	for _, test := range addressTestVectors {
		t.Run(test.address, func(t *testing.T) {
			testAddress(t, test)
		})
	}

	// Finally, test invalid inputs to see if they are handled correctly.
	for _, invalidAddr := range invalidTestVectors {
		t.Run(invalidAddr, func(t *testing.T) {
			testInvalidAddress(t, invalidAddr)
		})
	}
}

// testAddress parses an address from its string representation, and
// asserts that the parsed net.Addr is correct against the given test case.
func testAddress(t *testing.T, test addressTest) {
	addr := []string{test.address}
	normalized, err := NormalizeAddresses(
		addr, defaultTestPort, net.ResolveTCPAddr,
	)
	if err != nil {
		t.Fatalf("unable to normalize address %s: %v",
			test.address, err)
	}

	if len(addr) == 0 {
		t.Fatalf("no normalized addresses returned")
	}

	netAddr := normalized[0]
	validateAddr(t, netAddr, test)
}

// testInvalidAddress asserts that parsing the invalidAddr string using
// NormalizeAddresses results in an error.
func testInvalidAddress(t *testing.T, invalidAddr string) {
	addr := []string{invalidAddr}
	_, err := NormalizeAddresses(
		addr, defaultTestPort, net.ResolveTCPAddr,
	)
	if err == nil {
		t.Fatalf("expected error when parsing %v", invalidAddr)
	}
}

var (
	pubKeyBytes = []byte{0x03,
		0xc7, 0x82, 0x86, 0xd0, 0xbf, 0xe0, 0xb2, 0x33,
		0x77, 0xe3, 0x47, 0xd7, 0xd9, 0x63, 0x94, 0x3c,
		0x4f, 0x57, 0x5d, 0xdd, 0xd5, 0x7e, 0x2f, 0x1d,
		0x52, 0xa5, 0xbe, 0x1e, 0xb7, 0xf6, 0x25, 0xa4,
	}

	pubKeyHex = hex.EncodeToString(pubKeyBytes)

	pubKey, _ = btcec.ParsePubKey(pubKeyBytes)
)

type lnAddressCase struct {
	lnAddress      string
	expectedPubKey *btcec.PublicKey

	addressTest
}

// lnAddressTests constructs valid LNAddress test vectors from the existing set
// of valid address test vectors. All addresses will use the same public key for
// the positive tests.
var lnAddressTests = func() []lnAddressCase {
	var cases []lnAddressCase
	for _, addrTest := range addressTestVectors {
		cases = append(cases, lnAddressCase{
			lnAddress:      pubKeyHex + "@" + addrTest.address,
			expectedPubKey: pubKey,
			addressTest:    addrTest,
		})
	}

	return cases
}()

var invalidLNAddressTests = []string{
	"",                                  // empty string
	"@",                                 // empty pubkey
	"nonhexpubkey@",                     // non-hex public key
	pubKeyHex[:len(pubKeyHex)-2] + "@",  // pubkey too short
	pubKeyHex + "aa@",                   // pubkey too long
	pubKeyHex[:len(pubKeyHex)-1] + "7@", // pubkey not on curve
	pubKeyHex + "@some string",          // invalid address
	pubKeyHex + "@://",                  // invalid address
	pubKeyHex + "@21.21.21.21.21",       // invalid address
}

// TestLNAddresses performs both positive and negative tests against
// ParseLNAddressString.
func TestLNAddresses(t *testing.T) {
	for _, test := range lnAddressTests {
		t.Run(test.lnAddress, func(t *testing.T) {
			testLNAddress(t, test)
		})
	}

	for _, invalidAddr := range invalidLNAddressTests {
		t.Run(invalidAddr, func(t *testing.T) {
			testInvalidLNAddress(t, invalidAddr)
		})
	}
}

// testLNAddress parses an LNAddress from its string representation, and asserts
// that the parsed IdentityKey and Address are correct according to its test
// case.
func testLNAddress(t *testing.T, test lnAddressCase) {
	// Parse the LNAddress using the default port and TCP resolver.
	lnAddr, err := ParseLNAddressString(
		test.lnAddress, defaultTestPort, net.ResolveTCPAddr,
	)
	require.NoError(t, err, "unable to parse ln address")

	// Assert that the public key matches the expected public key.
	pkBytes := lnAddr.IdentityKey.SerializeCompressed()
	if !bytes.Equal(pkBytes, pubKeyBytes) {
		t.Fatalf("mismatched pubkey, want: %x, got: %v",
			pubKeyBytes, pkBytes)
	}

	// Assert that the address after the @ is parsed properly, as if it were
	// just a standalone address parsed by ParseAddressString.
	validateAddr(t, lnAddr.Address, test.addressTest)
}

// testLNAddressCase asserts that parsing the given invalidAddr string results
// in an error when parsed with ParseLNAddressString.
func testInvalidLNAddress(t *testing.T, invalidAddr string) {
	_, err := ParseLNAddressString(
		invalidAddr, defaultTestPort, net.ResolveTCPAddr,
	)
	if err == nil {
		t.Fatalf("expected error when parsing invalid lnaddress: %v",
			invalidAddr)
	}
}

// validateAddr asserts that an addr parsed by ParseAddressString matches the
// properties expected by its addressTest. In particular, it validates that the
// Network() and String() methods match the expectedNetwork and expectedAddress,
// respectively. Further, we test the IsLoopback and IsUnix detection methods
// against addr and assert that they match the expected values in the test case.
func validateAddr(t *testing.T, addr net.Addr, test addressTest) {

	t.Helper()

	// Assert that the parsed network and address match what we expect.
	if addr.Network() != test.expectedNetwork ||
		addr.String() != test.expectedAddress {
		t.Fatalf("mismatched address: expected %s://%s, "+
			"got %s://%s",
			test.expectedNetwork, test.expectedAddress,
			addr.Network(), addr.String(),
		)
	}

	// Assert whether we expect this address to be a loopback address.
	isAddrLoopback := IsLoopback(addr.String())
	if test.isLoopback != isAddrLoopback {
		t.Fatalf("mismatched loopback detection: expected "+
			"%v, got %v for addr %s",
			test.isLoopback, isAddrLoopback, test.address,
		)
	}

	// Assert whether we expect this address to be a unix address.
	isAddrUnix := IsUnix(addr)
	if test.isUnix != isAddrUnix {
		t.Fatalf("mismatched unix detection: expected "+
			"%v, got %v for addr %s",
			test.isUnix, isAddrUnix, test.address,
		)
	}
}

func TestIsPrivate(t *testing.T) {
	nonPrivateIPList := []net.IP{
		net.IPv4(169, 255, 0, 0),
		{0xfe, 0x79, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		net.IPv4(225, 0, 0, 0),
		{0xff, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		net.IPv4(11, 0, 0, 0),
		net.IPv4(172, 15, 0, 0),
		net.IPv4(192, 169, 0, 0),
		{0xfe, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		net.IPv4(8, 8, 8, 8),
		{2, 0, 0, 1, 4, 8, 6, 0, 4, 8, 6, 0, 8, 8, 8, 8},
	}
	privateIPList := []net.IP{
		net.IPv4(169, 254, 0, 0),
		{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		net.IPv4(224, 0, 0, 0),
		{0xff, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		net.IPv4(10, 0, 0, 1),
		net.IPv4(172, 16, 0, 1),
		net.IPv4(172, 31, 255, 255),
		net.IPv4(192, 168, 0, 1),
		{0xfc, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}

	testParams := []struct {
		name    string
		ipList  []net.IP
		private bool
	}{
		{
			"Non-private addresses should return false",
			nonPrivateIPList, false,
		},
		{
			"Private addresses should return true",
			privateIPList, true,
		},
	}

	for _, tt := range testParams {
		test := tt
		t.Run(test.name, func(t *testing.T) {
			for _, ip := range test.ipList {
				addr := &net.TCPAddr{IP: ip}
				require.Equal(
					t, test.private, IsPrivate(addr),
					"expected IP: %s to be %v", ip, test.private,
				)
			}
		})
	}
}
