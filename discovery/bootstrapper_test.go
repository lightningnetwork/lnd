package discovery

import (
	"context"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// stubNode is a minimal autopilot.Node implementation used to drive
// ChannelGraphBootstrapper from a unit test.
type stubNode struct {
	pub   *btcec.PublicKey
	addrs []net.Addr
}

func (n *stubNode) PubKey() [33]byte {
	var out [33]byte
	copy(out[:], n.pub.SerializeCompressed())
	return out
}

func (n *stubNode) Addrs() []net.Addr { return n.addrs }

// stubChannelGraph yields a fixed list of nodes from ForEachNode and is
// otherwise unused by SampleNodeAddrs.
type stubChannelGraph struct {
	nodes []autopilot.Node
}

// ForEachNode invokes the callback for each node in the stub graph,
// stopping early if the callback returns a non-nil error.
func (s *stubChannelGraph) ForEachNode(ctx context.Context,
	cb func(context.Context, autopilot.Node) error, _ func()) error {

	for _, n := range s.nodes {
		if err := cb(ctx, n); err != nil {
			return err
		}
	}

	return nil
}

// ForEachNodesChannels is a no-op stub; SampleNodeAddrs does not exercise
// the channel iteration path.
func (s *stubChannelGraph) ForEachNodesChannels(_ context.Context,
	_ func(context.Context, autopilot.Node,
		[]*autopilot.ChannelEdge) error, _ func()) error {

	return nil
}

// TestGraphBootstrapperSkipsV2Onion ensures SampleNodeAddrs strips Tor v2
// .onion entries from the returned candidate set so the connection manager
// never attempts to dial an obsolete v2 hidden service surfaced through the
// channel graph. A node whose remaining addresses include a v3 .onion or
// plain TCP entry is still returned for those addresses, and a node whose
// only address is v2 contributes nothing.
func TestGraphBootstrapperSkipsV2Onion(t *testing.T) {
	t.Parallel()

	v2 := &tor.OnionAddr{
		OnionService: "3g2upl4pq6kufc4m.onion",
		Port:         9735,
	}
	v3 := &tor.OnionAddr{
		OnionService: "4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnz" +
			"jpbi7utijcltosqemad.onion",
		Port: 9735,
	}
	tcp := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9735}

	mkPub := func(t *testing.T) *btcec.PublicKey {
		t.Helper()
		priv, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return priv.PubKey()
	}

	mixedPub := mkPub(t)
	v2OnlyPub := mkPub(t)

	graph := &stubChannelGraph{
		nodes: []autopilot.Node{
			&stubNode{
				pub:   mixedPub,
				addrs: []net.Addr{v2, v3, tcp},
			},
			&stubNode{
				pub:   v2OnlyPub,
				addrs: []net.Addr{v2},
			},
		},
	}

	// Use deterministic sampling so the hash accumulator never skips a
	// node — every eligible address must surface on each call.
	bs, err := NewGraphBootstrapper(graph, true)
	require.NoError(t, err)

	// Ask for more addresses than the graph holds so the bootstrapper
	// drains it in a single pass.
	got, err := bs.SampleNodeAddrs(
		t.Context(), 10, map[autopilot.NodeID]struct{}{},
	)
	require.NoError(t, err)

	// Collect (pubkey, address) pairs we expect — the v2-only node must
	// not contribute any NetAddress, and the mixed node contributes only
	// v3 and tcp.
	type seenAddr struct {
		pub  [33]byte
		addr string
	}
	seen := make(map[seenAddr]struct{}, len(got))
	for _, na := range got {
		var p [33]byte
		copy(p[:], na.IdentityKey.SerializeCompressed())
		seen[seenAddr{pub: p, addr: na.Address.String()}] = struct{}{}
	}

	var mixedKey, v2OnlyKey [33]byte
	copy(mixedKey[:], mixedPub.SerializeCompressed())
	copy(v2OnlyKey[:], v2OnlyPub.SerializeCompressed())

	// Mixed node yields v3 and tcp entries.
	require.Contains(t, seen, seenAddr{
		pub: mixedKey, addr: v3.String(),
	})
	require.Contains(t, seen, seenAddr{
		pub: mixedKey, addr: tcp.String(),
	})

	// No v2 entry surfaces, for either node.
	for s := range seen {
		require.NotEqual(t, v2.String(), s.addr,
			"v2 onion %v leaked into candidate set", s.addr)
		require.NotEqual(t, v2OnlyKey, s.pub,
			"v2-only node %x should contribute nothing", s.pub)
	}
}
