package lnwire

import (
	"encoding/hex"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec"
)

func TestNetAddressDisplay(t *testing.T) {
	t.Parallel()

	pubKeyStr := "036a0c5ea35df8a528b98edf6f290b28676d51d0fe202b073fe677612a39c0aa09"
	pubHex, err := hex.DecodeString(pubKeyStr)
	if err != nil {
		t.Fatalf("unable to decode str: %v", err)
	}

	pubKey, err := btcec.ParsePubKey(pubHex, btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	addr, _ := net.ResolveTCPAddr("tcp", "10.0.0.2:9000")

	netAddr := NetAddress{
		IdentityKey: pubKey,
		Address:     addr,
	}

	if addr.Network() != netAddr.Network() {
		t.Fatalf("network addr mismatch: %v", err)
	}

	expectedAddr := pubKeyStr + "@" + addr.String()
	addrString := netAddr.String()
	if expectedAddr != addrString {
		t.Fatalf("expected %v, got %v", expectedAddr, addrString)
	}
}
