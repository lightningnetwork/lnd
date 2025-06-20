package itest

import (
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

var peerConnTestCases = []*lntest.TestCase{
	{
		Name:     "restricted on inbound",
		TestFunc: testPeerConnRestrictedOnInbound,
	},
	{
		Name:     "already connected",
		TestFunc: testPeerConnAlreadyConnected,
	},
	{
		Name:     "unlimited outbound",
		TestFunc: testPeerConnUnlimitedOutbound,
	},
	{
		Name:     "upgrade access perm",
		TestFunc: testPeerConnUpgradeAccessPerm,
	},
	{
		Name:     "node restart",
		TestFunc: testPeerConnNodeRestart,
	},
	{
		Name:     "peer reconnect",
		TestFunc: testPeerConnPeerReconnect,
	},
}

// testPeerConnRestrictedOnInbound checks that when the `num-restricted-slots`
// is reached, no more inbound connection is allowed. In addition, when a slot
// becomes available, new inbound connection should be allowed.
func testPeerConnRestrictedOnInbound(ht *lntest.HarnessTest) {
	args := []string{"--num-restricted-slots=1"}

	// Create a new node with only one slot available.
	alice := ht.NewNode("Alice", args)

	// Create two nodes Bob and Carol. Bob will connect to Alice first,
	// taking up Alice's only slot, and we expect the connection from Carol
	// to Alice to be failed.
	bob := ht.NewNode("Bob", nil)
	carol := ht.NewNode("Carol", nil)

	// Bob now connects to Alice - from Alice's PoV, this is her inbound
	// connection from Bob. We expect Bob to connect to Alice successfully.
	_, err := ht.ConnectNodesNoAssert(bob, alice)
	require.NoError(ht, err)
	ht.AssertConnected(alice, bob)

	// Carol now connects to Alice - from Alice's PoV, this is her inbound
	// connection from Carol. Carol's connection should be failed.
	_, err = ht.ConnectNodesNoAssert(carol, alice)
	require.NoError(ht, err)
	ht.AssertNotConnected(alice, carol)

	// Bob now disconnects, which will free up Alice's slot.
	ht.DisconnectNodes(bob, alice)

	// Carol connects again - since Alice's slot is freed, this connection
	// should succeed.
	_, err = ht.ConnectNodesNoAssert(carol, alice)
	require.NoError(ht, err)

	ht.AssertConnected(alice, carol)
	ht.AssertNotConnected(alice, bob)
}

// testPeerConnAlreadyConnected checks that when there's already a connection
// alive, another attempt to connect doesn't take up the slot or kill the
// existing connection, in specific,
//   - When Alice has an inbound connection from Bob, another inbound connection
//     from Bob will be noop.
//   - When Bob has an outbound connection to Alice, another outbound connection
//     to Alice will be noop.
//
// In this test we will create nodes using the dev flag `unsafeconnect` to mimic
// the behaviour of persistent/permanent peer connections, where it's likely
// inbound and outbound connections are happening at the same time.
func testPeerConnAlreadyConnected(ht *lntest.HarnessTest) {
	args := []string{
		"--num-restricted-slots=1",
	}

	// Create a new node with two slots available.
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", []string{"--dev.unsafeconnect"})
	carol := ht.NewNode("Carol", nil)

	// Bob now connects to Alice - from Alice's PoV, this is her inbound
	// connection from Bob. We expect Bob to connect to Alice successfully.
	_, err := ht.ConnectNodesNoAssert(bob, alice)
	require.NoError(ht, err)
	ht.AssertConnected(alice, bob)

	// Assert Alice's slot has been filled up by connecting Carol to Alice.
	_, err = ht.ConnectNodesNoAssert(carol, alice)
	require.NoError(ht, err)
	ht.AssertNotConnected(alice, carol)

	// Bob connects to Alice again - from Alice's PoV, she already has an
	// inbound connection from Bob so this connection attempt should be
	// noop. We expect Alice and Bob to stay connected.
	//
	// TODO(yy): There's no way to assert the connection stays the same atm,
	// need to update the RPC to return the socket port. As for now we need
	// to visit the logs to check the connection is the same or not.
	_, err = ht.ConnectNodesNoAssert(bob, alice)
	require.NoError(ht, err)
	ht.AssertConnected(alice, bob)
}

// testPeerConnUnlimitedOutbound checks that for outbound connections, they are
// not restricted by the `num-restricted-slots` config.
func testPeerConnUnlimitedOutbound(ht *lntest.HarnessTest) {
	args := []string{"--num-restricted-slots=1"}

	// Create a new node with one slot available.
	alice := ht.NewNode("Alice", args)

	// Create three nodes. Alice will have an inbound connection with Bob
	// and outbound connections with Carol and Dave.
	bob := ht.NewNode("Bob", args)
	carol := ht.NewNode("Carol", args)
	dave := ht.NewNode("Dave", args)

	// Bob now connects to Alice - from Alice's PoV, this is her inbound
	// connection from Bob. We expect Bob to connect to Alice successfully.
	_, err := ht.ConnectNodesNoAssert(bob, alice)
	require.NoError(ht, err)
	ht.AssertConnected(alice, bob)

	// Assert Alice's slot has been filled up by connecting Carol to Alice.
	_, err = ht.ConnectNodesNoAssert(carol, alice)
	require.NoError(ht, err)
	ht.AssertNotConnected(alice, carol)

	// Now let Alice make an outbound connection to Carol and assert it's
	// succeeded.
	_, err = ht.ConnectNodesNoAssert(alice, carol)
	require.NoError(ht, err)
	ht.AssertConnected(alice, carol)

	// Alice can also make an outbound connection to Dave and assert it's
	// succeeded since outbound connection is not restricted.
	_, err = ht.ConnectNodesNoAssert(alice, dave)
	require.NoError(ht, err)
	ht.AssertConnected(alice, dave)
}

// testPeerConnUpgradeAccessPerm checks that when a peer has, or used to have a
// channel with Alice, it won't use her restricted slots.
func testPeerConnUpgradeAccessPerm(ht *lntest.HarnessTest) {
	args := []string{"--num-restricted-slots=1"}

	// Create a new node with one slot available.
	alice := ht.NewNodeWithCoins("Alice", args)
	bob := ht.NewNode("Bob", nil)
	carol := ht.NewNodeWithCoins("Carol", nil)
	dave := ht.NewNode("Dave", nil)
	eve := ht.NewNode("Eve", nil)

	// Connect Bob to Alice, which will use Alice's available slot.
	ht.ConnectNodes(bob, alice)

	// Assert Alice's slot has been filled up by connecting Carol to Alice.
	_, err := ht.ConnectNodesNoAssert(carol, alice)
	require.NoError(ht, err)
	ht.AssertNotConnected(alice, carol)

	// Open a channel from Alice to Bob and let it stay pending.
	p := lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	pendingUpdate := ht.OpenChannelAssertPending(alice, bob, p)
	cpAB := lntest.ChanPointFromPendingUpdate(pendingUpdate)

	// Connect Carol to Alice - since Bob now has a pending channel with
	// Alice, there's a slot available for Carol to connect.
	ht.ConnectNodes(carol, alice)

	// Open a channel from Carol to Alice and let it stay pending.
	pendingUpdate = ht.OpenChannelAssertPending(carol, alice, p)
	cpCA := lntest.ChanPointFromPendingUpdate(pendingUpdate)

	// Mine the funding txns.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Dave should be able to connect to Alice.
	ht.ConnectNodes(dave, alice)

	// Open a channel from Alice to Dave, which will free up Alice's slot.
	cpAD := ht.OpenChannel(alice, dave, p)

	// Close the channels.
	ht.CloseChannel(bob, cpAB)
	ht.CloseChannel(carol, cpCA)
	ht.CloseChannel(dave, cpAD)

	// Alice should have one slot available, connect Eve to Alice now.
	ht.ConnectNodes(eve, alice)
}

// testPeerConnNodeRestart checks that when a peer has or used to have a channel
// with Alice, when Alice restarts, she should still have the available slot for
// more inbound connections.
func testPeerConnNodeRestart(ht *lntest.HarnessTest) {
	args := []string{"--num-restricted-slots=1"}

	// Create a new node with one slot available.
	alice := ht.NewNodeWithCoins("Alice", args)
	bob := ht.NewNode("Bob", []string{"--dev.unsafeconnect"})
	carol := ht.NewNode("Carol", nil)

	// Connect Bob to Alice, which will use Alice's available slot.
	ht.ConnectNodes(bob, alice)

	// Assert Alice's slot has been filled up by connecting Carol to Alice.
	_, err := ht.ConnectNodesNoAssert(carol, alice)
	require.NoError(ht, err)
	ht.AssertNotConnected(alice, carol)

	// Restart Alice, which will reset her current slots, allowing Bob to
	// connect to her again.
	ht.RestartNode(alice)
	ht.ConnectNodes(bob, alice)

	// Open a channel from Alice to Bob and let it stay pending.
	p := lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	pendingUpdate := ht.OpenChannelAssertPending(alice, bob, p)
	cp := lntest.ChanPointFromPendingUpdate(pendingUpdate)

	// Restart Alice and let Bob connect to her - since Bob has a pending
	// channel, it shouldn't take any slot.
	ht.RestartNode(alice)
	ht.ConnectNodes(bob, alice)

	// Connect Carol to Alice since Alice has a free slot.
	ht.ConnectNodes(carol, alice)

	// Mine the funding tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Restart Alice and let Bob connect to her - since Bob has an open
	// channel, it shouldn't take any slot.
	ht.RestartNode(alice)
	ht.ConnectNodes(bob, alice)

	// Connect Carol to Alice since Alice has a free slot.
	ht.ConnectNodes(carol, alice)

	// Close the channel.
	ht.CloseChannel(alice, cp)

	// Restart Alice and let Bob connect to her - since Bob has a closed
	// channel, it shouldn't take any slot.
	ht.RestartNode(alice)
	ht.ConnectNodes(bob, alice)

	// Connect Carol to Alice since Alice has a free slot.
	ht.ConnectNodes(carol, alice)
}

// testPeerConnPeerReconnect checks that when a peer has or used to have a
// channel with Alice, it will not account for the restricted slot during
// reconnection.
func testPeerConnPeerReconnect(ht *lntest.HarnessTest) {
	args := []string{"--num-restricted-slots=1"}

	// Create a new node with one slot available.
	alice := ht.NewNodeWithCoins("Alice", args)
	bob := ht.NewNode("Bob", []string{"--dev.unsafeconnect"})
	carol := ht.NewNode("Carol", nil)

	// Connect Bob to Alice, which will use Alice's available slot.
	ht.ConnectNodes(bob, alice)

	// Let Bob connect to Alice again, which put Bob in Alice's
	// `scheduledPeerConnection` map.
	ht.ConnectNodes(bob, alice)

	// Assert Alice's slot has been filled up by connecting Carol to Alice.
	_, err := ht.ConnectNodesNoAssert(carol, alice)
	require.NoError(ht, err)
	ht.AssertNotConnected(alice, carol)

	// Open a channel from Alice to Bob and let it stay pending.
	p := lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	pendingUpdate := ht.OpenChannelAssertPending(alice, bob, p)
	cp := lntest.ChanPointFromPendingUpdate(pendingUpdate)

	// Bob now perform a reconnection - since Bob has a pending channel, it
	// shouldn't take any slot.
	ht.DisconnectNodes(bob, alice)
	ht.AssertNotConnected(alice, bob)
	ht.ConnectNodes(bob, alice)

	// Connect Carol to Alice since Alice has a free slot.
	ht.ConnectNodes(carol, alice)

	// Once the above connection succeeded we let Carol disconnect to free
	// the slot.
	ht.DisconnectNodes(carol, alice)

	// Mine the funding tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Bob now perform a reconnection - since Bob has a pending channel, it
	// shouldn't take any slot.
	ht.DisconnectNodes(bob, alice)
	ht.AssertNotConnected(alice, bob)
	ht.ConnectNodes(bob, alice)

	// Connect Carol to Alice since Alice has a free slot.
	ht.ConnectNodes(carol, alice)

	// Once the above connection succeeded we let Carol disconnect to free
	// the slot.
	ht.DisconnectNodes(carol, alice)

	// Close the channel.
	ht.CloseChannel(alice, cp)

	// Bob now perform a reconnection - since Bob has a pending channel, it
	// shouldn't take any slot.
	ht.DisconnectNodes(bob, alice)
	ht.AssertNotConnected(alice, bob)
	ht.ConnectNodes(bob, alice)

	// Connect Carol to Alice since Alice has a free slot.
	ht.ConnectNodes(carol, alice)
}
