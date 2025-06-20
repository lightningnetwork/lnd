package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lntest"
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
	// 1) Set up the following node/channel network.
	// 	Alice <- Bob <- Charlie
	_, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil, nil}, lntest.OpenChannelParams{
			Amt: btcutil.Amount(100000),
		},
	)
	carol, bob, alice := nodes[0], nodes[1], nodes[2]

	// Assert that they all know about each other and the channels.
	ht.AssertNumEdges(alice, 2, false)
	ht.AssertNumEdges(bob, 2, false)
	ht.AssertNumEdges(carol, 2, false)

	// Spin up a new node, Dave.
	dave := ht.NewNode("Dave", nil)

	// Explicitly assert that Dave was started with bootstrapping disabled.
	require.False(ht, dave.Cfg.WithPeerBootstrap)

	// Assert that Dave's current view of the graph is empty.
	ht.AssertNumEdges(dave, 0, false)

	// Now, connect Dave to Alice and wait for gossip sync to complete.
	ht.EnsureConnected(dave, alice)
	ht.AssertNumEdges(dave, 2, false)

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
	require.NoError(ht, err)

	// Dave should still know about the full graph though since it was
	// synced previously from Alice.
	ht.AssertNumEdges(dave, 2, false)

	// Restart Dave again but this time with bootstrapping enabled.
	dave.Cfg.WithPeerBootstrap = true
	ht.RestartNode(dave)

	// Show that Dave now does connect to some peers. The default minimum
	// number of peers that will be connected to is 3, so we
	// expect Dave to connect to all three nodes in the network.
	ht.AssertConnected(dave, alice)
	ht.AssertConnected(dave, bob)
	ht.AssertConnected(dave, carol)
}
