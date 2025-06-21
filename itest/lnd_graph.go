package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testPeerBootstrapping tests that a node is able to use persisted node
// announcements to bootstrap its peer connections. This is done by
// connecting a node to a channel network so that it syncs the necessary gossip
// and then:
//  1. Asserting that on a restart where bootstrapping is _disabled_, the node
//     does not connect to any peers.
//  2. Asserting that on a restart where bootstrapping is _enabled_, the node
//     does connect to peers.
func testPeerBootstrapping(ht *lntest.HarnessTest) {
	var descGraphReq = &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}

	// assertDescGraph is a helper that we can use to assert that a node
	// knows about a certain number of edges and nodes in the graph.
	assertDescGraph := func(node *node.HarnessNode, numEdges int,
		nodes ...*node.HarnessNode) {

		descGraphResp := node.RPC.DescribeGraph(descGraphReq)

		// Assert that the node knows about the expected number of
		// edges.
		require.Len(ht.T, descGraphResp.Edges, numEdges)

		// Make a lookup table of known nodes.
		knownNodes := map[string]bool{
			// A node will always know about itself.
			node.PubKeyStr: true,
		}
		for _, n := range descGraphResp.Nodes {
			knownNodes[n.PubKey] = true
		}
		require.Len(ht.T, knownNodes, len(nodes)+1)

		// Assert that all the expected nodes are known.
		for _, n := range nodes {
			require.True(ht.T, knownNodes[n.PubKeyStr])
		}
	}

	// 1) Set up the following node/channel network.
	// 	Alice <- Bob <- Charlie
	_, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil, nil}, lntest.OpenChannelParams{
			Amt: btcutil.Amount(100000),
		},
	)
	carol, bob, alice := nodes[0], nodes[1], nodes[2]

	// Assert that they all know about each other and the channels.
	assertDescGraph(alice, 2, bob, carol)
	assertDescGraph(bob, 2, alice, carol)
	assertDescGraph(carol, 2, alice, bob)

	// Spin up a new node, Dave.
	dave := ht.NewNode("Dave", nil)

	// Explicitly assert that Dave was started with bootstrapping disabled.
	require.False(ht.T, dave.Cfg.WithPeerBootstrap)

	// Assert that Dave's current view of the graph is empty.
	assertDescGraph(dave, 0)

	// Now, connect Dave to Alice and wait for gossip sync to complete.
	ht.EnsureConnected(dave, alice)
	assertDescGraph(dave, 2, alice, bob, carol)

	// Disconnect Dave from Alice and restart Dave. Since Alice and Dave
	// did not have channels between them, the nodes should not reconnect
	// to each other.
	ht.DisconnectNodes(dave, alice)
	ht.RestartNode(dave)

	// Dave still has bootstrapping disabled and so it should not connect
	// to any peers.
	err := wait.Invariant(func() bool {
		peerResp := dave.RPC.ListPeers()

		return len(peerResp.Peers) == 0
	}, time.Second*5)
	require.NoError(ht.T, err)

	// Dave should still know about the full graph though since it was
	// synced previously from Alice.
	assertDescGraph(dave, 2, alice, bob, carol)

	// Restart Dave again but this time with bootstrapping enabled.
	dave.Cfg.WithPeerBootstrap = true
	ht.RestartNode(dave)

	// Show that Dave now does connect to some peers.
	err = wait.Predicate(func() bool {
		peerResp := dave.RPC.ListPeers()

		return len(peerResp.Peers) > 0
	}, time.Second*5)
	require.NoError(ht.T, err)
}
