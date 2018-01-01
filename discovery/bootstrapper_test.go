package discovery

import (
	"net"
	"testing"

	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/roasbeef/btcd/btcec"
)

// mock autopilot.Node type for testing, with no channels
type mockNode struct {
	pubKey *btcec.PublicKey
	addrs  []net.Addr
}

// compile time assertion to ensure that mockNode meets the
// autopilot.Node interface without any channels
var _ autopilot.Node = (*mockNode)(nil)

func (n *mockNode) PubKey() *btcec.PublicKey {
	return n.pubKey
}

func (n *mockNode) Addrs() []net.Addr {
	return n.addrs[:]
}

func (n *mockNode) ForEachChannel(_ func(autopilot.ChannelEdge) error) error {
	// no-op
	return nil
}

// mock autopilot.ChannelGraph type for testing
type mockChannelGraph struct {
	nodes []autopilot.Node
}

func (m *mockChannelGraph) ForEachNode(f func(autopilot.Node) error) error {
	// call f() for each node
	for _, n := range m.nodes {
		if err := f(n); err != nil {
			return err
		}
	}
	return nil
}

// TestNewChannelGraphBootstrapper checks that a new ChannelGraphBootstrapper
// can be created
func TestNewChannelGraphBootstrapper(t *testing.T) {
	channGraph := &mockChannelGraph{
		nodes: nil,
	}

	cgb, err := NewGraphBootstrapper(channGraph)
	if err != nil {
		t.Fatalf("can't initialize bootstrapper: %v", err)
	}
	if cgb == nil {
		t.Fatalf("bootstrapper was nil")
	}
}

// TestChannelGraphBootstrapOneNode ensures that a graph one node which has
// one address will be selected by the ChannelGraphBootstraper
func TestChannelGraphBootstrapOneNode(t *testing.T) {
	nodePrivKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to make private key: %v", err)
	}

	channGraph := &mockChannelGraph{
		nodes: []autopilot.Node{&mockNode{
			pubKey: nodePrivKey.PubKey(),
			addrs: []net.Addr{&net.TCPAddr{
				IP:   (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
				Port: 9000},
			},
		}},
	}

	cgb, err := NewGraphBootstrapper(channGraph)
	if err != nil {
		t.Fatalf("can't initialize bootstrapper: %v", err)
	}

	addrs, err := cgb.SampleNodeAddrs(1, nil)
	if err != nil {
		t.Fatalf("failed to sample nodes: %v", err)
	}
	if len(addrs) != 1 {
		t.Fatalf("expected 1 address, but got %d", len(addrs))
	}
}

// TestChannelGraphBootstrapOneNodeManyAddrs ensures that in case of a lack of
// nodes to fulfill a ChannelGraphBootstraper sampling request, alternative
// addresses for known nodes are used to fill the short.
func TestChannelGraphBootstrapOneNodeManyAddrs(t *testing.T) {
	nodePrivKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to make private key: %v", err)
	}

	channGraph := &mockChannelGraph{
		nodes: []autopilot.Node{&mockNode{
			pubKey: nodePrivKey.PubKey(),
			addrs: []net.Addr{
				&net.TCPAddr{
					IP:   (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
					Port: 9000,
				},
				&net.TCPAddr{
					IP:   (net.IP)([]byte{0xA, 0x0, 0x0, 0x2}),
					Port: 9000,
				},
				&net.TCPAddr{
					IP:   (net.IP)([]byte{0xA, 0x0, 0x0, 0x3}),
					Port: 9000,
				},
			},
		}},
	}

	cgb, err := NewGraphBootstrapper(channGraph)
	if err != nil {
		t.Fatalf("can't initialize bootstrapper: %v", err)
	}

	addrs, err := cgb.SampleNodeAddrs(2, nil)
	if err != nil {
		t.Fatalf("failed to sample nodes: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("expected 2 address, but got %d", len(addrs))
	}
}

// TestChannelGraphBootstrapManyNodesOneAddr checks that repeated sampling
// of a small set of nodes will both produce the correct number requested and
// eventually produce all of the nodes.
func TestChannelGraphBootstrapManyNodesOneAddr(t *testing.T) {
	nodePrivKey1, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to make private key 1: %v", err)
	}
	nodePrivKey2, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to make private key 2: %v", err)
	}
	nodePrivKey3, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to make private key 3: %v", err)
	}

	channGraph := &mockChannelGraph{
		nodes: []autopilot.Node{
			&mockNode{
				pubKey: nodePrivKey1.PubKey(),
				addrs: []net.Addr{
					&net.TCPAddr{
						IP:   (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
						Port: 9000,
					},
				},
			},
			&mockNode{
				pubKey: nodePrivKey2.PubKey(),
				addrs: []net.Addr{
					&net.TCPAddr{
						IP:   (net.IP)([]byte{0xA, 0x0, 0x0, 0x2}),
						Port: 9000,
					},
				},
			},
			&mockNode{
				pubKey: nodePrivKey3.PubKey(),
				addrs: []net.Addr{
					&net.TCPAddr{
						IP:   (net.IP)([]byte{0xA, 0x0, 0x0, 0x3}),
						Port: 9000,
					},
				},
			},
		},
	}

	cgb, err := NewGraphBootstrapper(channGraph)
	if err != nil {
		t.Fatalf("can't initialize bootstrapper: %v", err)
	}

	addrs, err := cgb.SampleNodeAddrs(2, nil)
	if err != nil {
		t.Fatalf("failed to sample nodes: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("expected 2 address, but got %d", len(addrs))
	}

	// ensure that another selection will return the remaining address
	addr2, err := cgb.SampleNodeAddrs(1, nil)
	if err != nil {
		t.Fatalf("Failed to sample nodes: %v", err)
	}
	if len(addr2) != 1 {
		t.Fatalf("expected 1 address, but got %d", len(addrs))
	}
	// ensure that all addresses were discovered by making sure they're all here
	addrs = append(addrs, addr2...)
	for _, expectedNode := range channGraph.nodes {
		found := false
		for _, nodeAddr := range addrs {
			if expectedNode.Addrs()[0] == nodeAddr.Address {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf(
				"expected to see address %v, but did not",
				expectedNode.Addrs()[0],
			)
		}
	}
}
