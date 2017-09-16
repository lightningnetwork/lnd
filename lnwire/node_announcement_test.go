package lnwire

import (
	prand "math/rand"
	"net"
	"testing"
	"time"

	"github.com/roasbeef/btcd/btcec"
)

func TestCompareNodeAnnouncements(t *testing.T) {
	t.Parallel()

	randSource := prand.NewSource(time.Now().Unix())
	priv, _ := btcec.NewPrivateKey(btcec.S256())
	alias, _ := NewNodeAlias("kek")
	rgb := RGB{
		red:   uint8(prand.Int31()),
		green: uint8(prand.Int31()),
		blue:  uint8(prand.Int31()),
	}
	a1, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:9735")
	a2, _ := net.ResolveTCPAddr("tcp", "10.0.0.1:9000")


	// Create a node announcement with the dummy data above
	node1 := &NodeAnnouncement{
		Features:  randFeatureVector(prand.New(randSource)),
		Alias:     alias,
		RGBColor:  rgb,
		Addresses: []net.Addr{a2, a1},
		NodeID:    priv.PubKey(),
	}

	// Create a copy of node1, but switch the order of the
	// addresses array. Compare nodes should still return
	// true because the order of the addresses don't matter
	node2 := new (NodeAnnouncement)

	*node2 = *node1
	node2.Addresses = []net.Addr{a1, a2}
	if !node1.CompareNodes(node2) {
		t.Fatalf("expected node comparison to return true")
	}

	// Ensure CompareNodes returns false when the nodes' feature
	// vectors are not equal.
	*node2 = *node1
	node2.Features = randFeatureVector(prand.New(randSource))
	if node1.CompareNodes(node2) {
		t.Fatalf("expected node comparison to return false")
	}

	// Ensure CompareNodes returns false when the nodes' 
	// aliases are not equal.
	*node2 = *node1
	alias2, _ := NewNodeAlias("kek2")
	node2.Alias = alias2
	if node1.CompareNodes(node2) {
		t.Fatalf("expected node comparison to return false")
	}
	
	// Ensure CompareNodes returns false when the nodes' 
	// RGBColors are not equal.
	*node2 = *node1
	node2.RGBColor = RGB{
		red:   uint8(prand.Int31()),
		green: uint8(prand.Int31()),
		blue:  uint8(prand.Int31()),
	}
	if node1.CompareNodes(node2) {
		t.Fatalf("expected node comparison to return false")
	}

	// Ensure CompareNodes returns false when the nodes' 
	// address arrays are not fully equal.
	*node2 = *node1
	a3, _ := net.ResolveTCPAddr("tcp", "10.0.0.1:9001")
	node2.Addresses = []net.Addr{a2, a3}
	if node1.CompareNodes(node2) {
		t.Fatalf("expected node comparison to return false")
	}
	
	// Ensure CompareNodes returns false when the nodes' 
	// public keys are not equal.
	*node2 = *node1
	priv2, _ := btcec.NewPrivateKey(btcec.S256())
	node2.NodeID = priv2.PubKey()
	if node1.CompareNodes(node2) {
		t.Fatalf("expected node comparison to return false")
	}
}