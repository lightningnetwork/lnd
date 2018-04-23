package lnwire

import (
	"image/color"
	"net"
	"testing"

	"github.com/roasbeef/btcd/btcec"
)

func TestCompareNodeAnnouncements(t *testing.T) {
	t.Parallel()

	// Two pointers that point to the same node announcement should be
	// equal.
	a := &NodeAnnouncement{}
	b := a
	if !a.IsEqual(b) {
		t.Fatal("expected node announcement comparison to return true")
	}

	// Two node announcements with different node public keys should not be
	// equal.
	privateKey1, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to create private key: %v", err)
	}
	var compressedPubKey1 [33]byte
	copy(compressedPubKey1[:], privateKey1.PubKey().SerializeCompressed())

	privateKey2, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to create private key: %v", err)
	}
	var compressedPubKey2 [33]byte
	copy(compressedPubKey2[:], privateKey2.PubKey().SerializeCompressed())

	a = &NodeAnnouncement{NodeID: compressedPubKey1}
	b = &NodeAnnouncement{NodeID: compressedPubKey2}
	if a.IsEqual(b) {
		t.Fatal("expected node announcement comparison to return false")
	}

	// Two node announcements with different colors should not be equal.
	color1 := color.RGBA{1, 2, 3, 4}
	color2 := color.RGBA{2, 3, 4, 5}

	a = &NodeAnnouncement{RGBColor: color1}
	b = &NodeAnnouncement{RGBColor: color2}
	if a.IsEqual(b) {
		t.Fatal("expected node announcement comparison to return false")
	}

	// Two node announcements with different aliases should not be equal.
	alias1, _ := NewNodeAlias("lol")
	alias2, _ := NewNodeAlias("kek")

	a = &NodeAnnouncement{Alias: alias1}
	b = &NodeAnnouncement{Alias: alias2}
	if a.IsEqual(b) {
		t.Fatal("expected node announcement comparison to return false")
	}

	// Two node announcements with different addresses should not be equal.
	addr1, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:9735")
	addr2, _ := net.ResolveTCPAddr("tcp", "10.0.0.1:9000")

	a = &NodeAnnouncement{Addresses: []net.Addr{addr1}}
	b = &NodeAnnouncement{Addresses: []net.Addr{addr1, addr2}}
	if a.IsEqual(b) {
		t.Fatal("expected node announcement comparison to return false")
	}

	// Two node announcements with different feature bits set should not be
	// equal.
	features1 := NewRawFeatureVector(InitialRoutingSync)
	features2 := NewRawFeatureVector()

	a = &NodeAnnouncement{Features: features1}
	b = &NodeAnnouncement{Features: features2}
	if a.IsEqual(b) {
		t.Fatal("expected node announcement comparison to return false")
	}

	// Two node announcements that contain the same information should be
	// equal.
	a = &NodeAnnouncement{
		NodeID:    compressedPubKey1,
		RGBColor:  color1,
		Alias:     alias1,
		Addresses: []net.Addr{addr1},
		Features:  features1,
	}
	b = &NodeAnnouncement{
		NodeID:    compressedPubKey1,
		RGBColor:  color1,
		Alias:     alias1,
		Addresses: []net.Addr{addr1},
		Features:  features1,
	}
	if !a.IsEqual(b) {
		t.Fatal("expected node announcement comparison to return true")
	}
}
