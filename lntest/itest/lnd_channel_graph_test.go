package itest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testUpdateChanStatus checks that calls to the UpdateChanStatus RPC update
// the channel graph as expected, and that channel state is properly updated
// in the presence of interleaved node disconnects / reconnects.
func testUpdateChanStatus(ht *lntest.HarnessTest) {
	// Create two fresh nodes and open a channel between them.
	alice := ht.NewNode(
		"Alice", []string{
			"--minbackoff=10s",
			"--chan-enable-timeout=3s",
			"--chan-disable-timeout=4s",
			"--chan-status-sample-interval=.5s",
		},
	)
	defer ht.Shutdown(alice)

	bob := ht.NewNode(
		"Bob", []string{
			"--minbackoff=10s",
			"--chan-enable-timeout=3s",
			"--chan-disable-timeout=4s",
			"--chan-status-sample-interval=.5s",
		},
	)
	defer ht.Shutdown(bob)

	// Connect Alice to Bob.
	ht.ConnectNodes(alice, bob)

	// Give Alice some coins so she can fund a channel.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, alice)

	// Launch a node for Carol which will connect to Alice and Bob in order
	// to receive graph updates. This will ensure that the channel updates
	// are propagated throughout the network.
	carol := ht.NewNode("Carol", nil)
	defer ht.Shutdown(carol)

	// Connect both Alice and Bob to the new node Carol, so she can sync
	// her graph.
	ht.ConnectNodes(alice, carol)
	ht.ConnectNodes(bob, carol)
	waitForGraphSync(ht, carol)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	//
	// NOTE: we need to open the channel after all nodes are synced such
	// that Alice and Bob won't time out while checking for channel
	// availability, as specified by the flag `--chan-disable-timeout`.
	chanAmt := btcutil.Amount(100000)
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(alice, chanPoint, false)

	// assertChannelUpdate checks that the required policy update has
	// happened in Carol's view.
	assertChannelUpdate := func(node *lntest.HarnessNode,
		policy *lnrpc.RoutingPolicy) {

		ht.AssertChannelPolicyUpdate(
			carol, node.PubKeyStr, policy, chanPoint, false,
		)
	}

	// sendReq sends an UpdateChanStatus request to the given node.
	sendReq := func(node *lntest.HarnessNode, chanPoint *lnrpc.ChannelPoint,
		action routerrpc.ChanStatusAction) {

		req := &routerrpc.UpdateChanStatusRequest{
			ChanPoint: chanPoint,
			Action:    action,
		}
		ht.UpdateChanStatus(node, req)
	}

	// assertEdgeDisabled ensures that Alice has the correct Disabled state
	// for given channel.
	// TODO(yy): refactor such that we can use ht.AssertChannelPolicy?
	assertEdgeDisabled := func(chanPoint *lnrpc.ChannelPoint,
		disabled bool) {

		outPoint := ht.OutPointFromChannelPoint(chanPoint)

		err := wait.NoError(func() error {
			chanGraph := ht.DescribeGraph(alice, true)
			numEdges := len(chanGraph.Edges)
			if numEdges != 1 {
				return fmt.Errorf("expected to find 1 edge in "+
					"the graph, found %d", numEdges)
			}
			edge := chanGraph.Edges[0]
			if edge.ChanPoint != outPoint.String() {
				return fmt.Errorf("expected chan_point %v, "+
					"got %v", outPoint, edge.ChanPoint)
			}
			var policy *lnrpc.RoutingPolicy
			if alice.PubKeyStr == edge.Node1Pub {
				policy = edge.Node1Policy
			} else {
				policy = edge.Node2Policy
			}
			if disabled != policy.Disabled {
				return fmt.Errorf("expected policy.Disabled "+
					"to be %v, but policy was %v", disabled,
					policy.Disabled)
			}

			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "assert edge disabled timeout")
	}

	// When updating the state of the channel between Alice and Bob, we
	// should expect to see channel updates with the default routing
	// policy. The value of "Disabled" will depend on the specific
	// scenario being tested.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(chainreg.DefaultBitcoinBaseFeeMSat),
		FeeRateMilliMsat: int64(chainreg.DefaultBitcoinFeeRate),
		TimeLockDelta:    chainreg.DefaultBitcoinTimeLockDelta,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      calculateMaxHtlc(chanAmt),
	}

	// Initially, the channel between Alice and Bob should not be
	// disabled. This check should happen right after the channel openning
	// as we've used a short timeout value for `--chan-disable-timeout`. If
	// we wait longer than that we might get a flake saying the channel is
	// disabled.
	assertEdgeDisabled(chanPoint, false)

	// Manually disable the channel and ensure that a "Disabled = true"
	// update is propagated.
	sendReq(alice, chanPoint, routerrpc.ChanStatusAction_DISABLE)
	expectedPolicy.Disabled = true
	assertChannelUpdate(alice, expectedPolicy)

	// Re-enable the channel and ensure that a "Disabled = false" update
	// is propagated.
	sendReq(alice, chanPoint, routerrpc.ChanStatusAction_ENABLE)
	expectedPolicy.Disabled = false
	assertChannelUpdate(alice, expectedPolicy)

	// Manually enabling a channel should NOT prevent subsequent
	// disconnections from automatically disabling the channel again
	// (we don't want to clutter the network with channels that are
	// falsely advertised as enabled when they don't work).
	ht.DisconnectNodes(alice, bob)
	expectedPolicy.Disabled = true
	assertChannelUpdate(alice, expectedPolicy)
	assertChannelUpdate(bob, expectedPolicy)

	// Reconnecting the nodes should propagate a "Disabled = false" update.
	ht.EnsureConnected(alice, bob)
	expectedPolicy.Disabled = false
	assertChannelUpdate(alice, expectedPolicy)
	assertChannelUpdate(bob, expectedPolicy)

	// Manually disabling the channel should prevent a subsequent
	// disconnect / reconnect from re-enabling the channel on
	// Alice's end. Note the asymmetry between manual enable and
	// manual disable!
	sendReq(alice, chanPoint, routerrpc.ChanStatusAction_DISABLE)

	// Alice sends out the "Disabled = true" update in response to
	// the ChanStatusAction_DISABLE request.
	expectedPolicy.Disabled = true
	assertChannelUpdate(alice, expectedPolicy)

	ht.DisconnectNodes(alice, bob)

	// Bob sends a "Disabled = true" update upon detecting the
	// disconnect.
	expectedPolicy.Disabled = true
	assertChannelUpdate(bob, expectedPolicy)

	// Bob sends a "Disabled = false" update upon detecting the
	// reconnect.
	ht.EnsureConnected(alice, bob)
	expectedPolicy.Disabled = false
	assertChannelUpdate(bob, expectedPolicy)

	// However, since we manually disabled the channel on Alice's end,
	// the policy on Alice's end should still be "Disabled = true". Again,
	// note the asymmetry between manual enable and manual disable!
	assertEdgeDisabled(chanPoint, true)

	ht.DisconnectNodes(alice, bob)

	// Bob sends a "Disabled = true" update upon detecting the
	// disconnect.
	expectedPolicy.Disabled = true
	assertChannelUpdate(bob, expectedPolicy)

	// After restoring automatic channel state management on Alice's end,
	// BOTH Alice and Bob should set the channel state back to "enabled"
	// on reconnect.
	sendReq(alice, chanPoint, routerrpc.ChanStatusAction_AUTO)
	ht.EnsureConnected(alice, bob)

	expectedPolicy.Disabled = false
	assertChannelUpdate(alice, expectedPolicy)
	assertChannelUpdate(bob, expectedPolicy)
	assertEdgeDisabled(chanPoint, false)
}

// testUnannouncedChannels checks unannounced channels are not returned by
// describeGraph RPC request unless explicitly asked for.
func testUnannouncedChannels(ht *lntest.HarnessTest) {
	amount := funding.MaxBtcFundingAmount
	alice, bob := ht.Alice(), ht.Bob()

	// Open a channel between Alice and Bob, ensuring the
	// channel has been opened properly.
	chanOpenUpdate := ht.OpenChannelStreamAndAssert(
		alice, bob, lntest.OpenChannelParams{Amt: amount},
	)

	// Mine 2 blocks, and check that the channel is opened but not yet
	// announced to the network.
	ht.MineBlocksAndAssertTx(2, 1)

	// One block is enough to make the channel ready for use, since the
	// nodes have defaultNumConfs=1 set.
	fundingChanPoint := ht.WaitForChannelOpen(chanOpenUpdate)

	// Alice should have 1 edge in her graph.
	ht.AssertNumEdges(alice, 1, true)

	// Channels should not be announced yet, hence Alice should have no
	// announced edges in her graph.
	ht.AssertNumEdges(alice, 0, false)

	// Mine 4 more blocks, and check that the channel is now announced.
	ht.MineBlocks(4)

	// Give the network a chance to learn that auth proof is confirmed.
	ht.AssertNumEdges(alice, 1, false)

	// Close the channel used during the test.
	ht.CloseChannel(alice, fundingChanPoint, false)
}

func testGraphTopologyNotifications(ht *lntest.HarnessTest) {
	ht.Run("pinned", func(t *testing.T) {
		subT := ht.Subtest(t)
		testGraphTopologyNtfns(subT, true)
	})
	ht.Run("unpinned", func(t *testing.T) {
		subT := ht.Subtest(t)
		testGraphTopologyNtfns(subT, false)
	})
}

func testGraphTopologyNtfns(ht *lntest.HarnessTest, pinned bool) {
	const chanAmt = funding.MaxBtcFundingAmount

	// Spin up Bob first, since we will need to grab his pubkey when
	// starting Alice to test pinned syncing.
	bob := ht.NewNode("bob", nil)
	defer ht.Shutdown(bob)

	bobInfo := ht.GetInfo(bob)
	bobPubkey := bobInfo.IdentityPubkey

	// For unpinned syncing, start Alice as usual. Otherwise grab Bob's
	// pubkey to include in his pinned syncer set.
	var aliceArgs []string
	if pinned {
		aliceArgs = []string{
			"--numgraphsyncpeers=0",
			fmt.Sprintf("--gossip.pinned-syncers=%s", bobPubkey),
		}
	}

	alice := ht.NewNode("alice", aliceArgs)
	defer ht.Shutdown(alice)

	// Connect Alice and Bob.
	ht.EnsureConnected(alice, bob)

	// Alice stimmy.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, alice)

	// Bob stimmy.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, bob)

	// Assert that Bob has the correct sync type before proceeding.
	if pinned {
		assertSyncType(ht, alice, bobPubkey, lnrpc.Peer_PINNED_SYNC)
	} else {
		assertSyncType(ht, alice, bobPubkey, lnrpc.Peer_ACTIVE_SYNC)
	}

	// Regardless of syncer type, ensure that both peers report having
	// completed their initial sync before continuing to make a channel.
	waitForGraphSync(ht, alice)

	// Open a new channel between Alice and Bob.
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// The channel opening above should have triggered a few notifications
	// sent to the notification client. We'll expect two channel updates,
	// and two node announcements.
	ht.AssertNumChannelUpdates(alice, chanPoint, 2)
	ht.AssertNumNodeAnns(alice, alice.PubKeyStr, 1)
	ht.AssertNumNodeAnns(alice, bob.PubKeyStr, 1)

	_, blockHeight := ht.GetBestBlock()

	// Now we'll test that updates are properly sent after channels are
	// closed within the network.
	ht.CloseChannel(alice, chanPoint, false)

	// Now that the channel has been closed, we should receive a
	// notification indicating so.
	closedChan := ht.AssertChannelClosedFromGraph(alice, chanPoint)

	require.Equal(ht, uint32(blockHeight+1), closedChan.ClosedHeight,
		"close heights of channel mismatch")

	fundingTxid := ht.OutPointFromChannelPoint(chanPoint)
	closeTxid := ht.OutPointFromChannelPoint(closedChan.ChanPoint)
	require.EqualValues(ht, fundingTxid, closeTxid,
		"channel point hash mismatch")

	// For the final portion of the test, we'll ensure that once a new node
	// appears in the network, the proper notification is dispatched. Note
	// that a node that does not have any channels open is ignored, so first
	// we disconnect Alice and Bob, open a channel between Bob and Carol,
	// and finally connect Alice to Bob again.
	ht.DisconnectNodes(alice, bob)

	carol := ht.NewNode("Carol", nil)
	defer ht.Shutdown(carol)

	ht.ConnectNodes(bob, carol)
	chanPoint = ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Reconnect Alice and Bob. This should result in the nodes syncing up
	// their respective graph state, with the new addition being the
	// existence of Carol in the graph, and also the channel between Bob
	// and Carol. Note that we will also receive a node announcement from
	// Bob, since a node will update its node announcement after a new
	// channel is opened.
	ht.EnsureConnected(alice, bob)

	// We should receive an update advertising the newly connected node,
	// Bob's new node announcement, and the channel between Bob and Carol.
	ht.AssertNumChannelUpdates(alice, chanPoint, 2)
	ht.AssertNumNodeAnns(alice, bob.PubKeyStr, 1)

	// Close the channel between Bob and Carol.
	ht.CloseChannel(bob, chanPoint, false)
}

// testNodeAnnouncement ensures that when a node is started with one or more
// external IP addresses specified on the command line, that those addresses
// announced to the network and reported in the network graph.
func testNodeAnnouncement(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice(), ht.Bob()

	advertisedAddrs := []string{
		"192.168.1.1:8333",
		"[2001:db8:85a3:8d3:1319:8a2e:370:7348]:8337",
		"bkb6azqggsaiskzi.onion:9735",
		"fomvuglh6h6vcag73xo5t5gv56ombih3zr2xvplkpbfd7wrog4swjwid.onion:1234",
	}

	var lndArgs []string
	for _, addr := range advertisedAddrs {
		lndArgs = append(lndArgs, "--externalip="+addr)
	}

	dave := ht.NewNode("Dave", lndArgs)
	defer ht.Shutdown(dave)

	// We must let Dave have an open channel before he can send a node
	// announcement, so we open a channel with Bob,
	ht.ConnectNodes(bob, dave)

	// We'll then go ahead and open a channel between Bob and Dave. This
	// ensures that Alice receives the node announcement from Bob as part of
	// the announcement broadcast.
	chanPoint := ht.OpenChannel(
		bob, dave, lntest.OpenChannelParams{Amt: 1000000},
	)

	assertAddrs := func(addrsFound []string, targetAddrs ...string) {
		addrs := make(map[string]struct{}, len(addrsFound))
		for _, addr := range addrsFound {
			addrs[addr] = struct{}{}
		}

		for _, addr := range targetAddrs {
			_, ok := addrs[addr]
			require.True(ht, ok, "address %v not found in node "+
				"announcement", addr)
		}
	}
	// We'll then wait for Alice to receive Dave's node announcement
	// including the expected advertised addresses from Bob since they
	// should already be connected.
	ht.AssertNumNodeAnns(alice, dave.PubKeyStr, 1)
	nodeUpdate := alice.GetNodeUpdates(dave.PubKeyStr)[0]
	assertAddrs(nodeUpdate.Addresses, advertisedAddrs...) // nolint:staticcheck

	// Close the channel between Bob and Dave.
	ht.CloseChannel(bob, chanPoint, false)
}

// waitForGraphSync is a helper function which waits until the node is
// synced to graph or times out.
//
// NOTE: only made for tests in this file.
func waitForGraphSync(ht *lntest.HarnessTest, hn *lntest.HarnessNode) {
	err := wait.NoError(func() error {
		resp := ht.GetInfo(hn)
		if resp.SyncedToGraph {
			return nil
		}

		return fmt.Errorf("node %s not synced to graph", hn.Name())
	}, defaultTimeout)
	require.NoError(ht, err, "wait for graph sync timeout")
}

// testUpdateNodeAnnouncement ensures that the RPC endpoint validates
// the requests correctly and that the new node announcement is brodcasted
// with the right information after updating our node.
func testUpdateNodeAnnouncement(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice(), ht.Bob()

	var lndArgs []string

	// Add some exta addresses to the default ones.
	extraAddrs := []string{
		"192.168.1.1:8333",
		"[2001:db8:85a3:8d3:1319:8a2e:370:7348]:8337",
		"bkb6azqggsaiskzi.onion:9735",
		"fomvuglh6h6vcag73xo5t5gv56ombih3zr2xvplkpbfd7wrog4swjwid.onion:1234",
	}
	for _, addr := range extraAddrs {
		lndArgs = append(lndArgs, "--externalip="+addr)
	}
	dave := ht.NewNode("Dave", lndArgs)
	defer ht.Shutdown(dave)

	// assertNodeAnn is a helper closure that checks a given node update
	// from Dave is seen by Alice.
	assertNodeAnn := func(expected *lnrpc.NodeUpdate) {
		err := wait.NoError(func() error {
			// Get a list of node updates seen by Alice.
			updates := alice.GetNodeUpdates(dave.PubKeyStr)

			// Check at least one of the updates matches the given
			// node update.
			for _, update := range updates {
				err := compareNodeAnns(update, expected)
				// Found a match, return nil.
				if err == nil {
					return nil
				}
			}

			// We've check all the updates and no match found.
			return fmt.Errorf("alice didn't see the update: %v",
				expected)
		}, defaultTimeout)

		require.NoError(ht, err, "assertNodeAnn failed")
	}

	// Get dave default information so we can compare it lately with the
	// brodcasted updates.
	resp := ht.GetInfo(dave)
	defaultAddrs := make([]*lnrpc.NodeAddress, 0, len(resp.Uris))
	for _, uri := range resp.GetUris() {
		values := strings.Split(uri, "@")
		defaultAddrs = append(
			defaultAddrs, &lnrpc.NodeAddress{
				Addr:    values[1],
				Network: "tcp",
			},
		)
	}

	// This feature bit is used to test that our endpoint sets/unsets
	// feature bits properly. If the current FeatureBit is set by default
	// update this one for another one unset by default at random.
	featureBit := lnrpc.FeatureBit_WUMBO_CHANNELS_REQ
	featureIdx := uint32(featureBit)
	_, ok := resp.Features[featureIdx]
	require.False(ht, ok, "unexpected feature bit enabled by default")

	defaultDaveNodeAnn := &lnrpc.NodeUpdate{
		Alias:         resp.Alias,
		Color:         resp.Color,
		NodeAddresses: defaultAddrs,
	}

	// Dave must have an open channel before he can send a node
	// announcement, so we open a channel with Bob.
	ht.ConnectNodes(bob, dave)

	// Go ahead and open a channel between Bob and Dave. This
	// ensures that Alice receives the node announcement from Bob as part of
	// the announcement broadcast.
	chanPoint := ht.OpenChannel(
		bob, dave,
		lntest.OpenChannelParams{
			Amt: 1000000,
		},
	)

	// Wait for Alice to receive dave's node announcement with the default
	// values.
	assertNodeAnn(defaultDaveNodeAnn)

	// We cannot differentiate between requests with Alias = "" and
	// requests that do not provide that field. If a user sets Alias = ""
	// in the request the field will simply be ignored. The request must
	// fail because no modifiers are applied.
	invalidNodeAnnReq := &peersrpc.NodeAnnouncementUpdateRequest{
		Alias: "",
	}
	ht.UpdateNodeAnnouncementErr(dave, invalidNodeAnnReq)

	// Alias too long.
	invalidNodeAnnReq = &peersrpc.NodeAnnouncementUpdateRequest{
		Alias: strings.Repeat("a", 50),
	}
	ht.UpdateNodeAnnouncementErr(dave, invalidNodeAnnReq)

	// Update Node.
	newAlias := "new-alias"
	newColor := "#2288ee"

	newAddresses := []string{
		"192.168.1.10:8333",
		"192.168.1.11:8333",
	}

	updateAddressActions := []*peersrpc.UpdateAddressAction{
		{
			Action:  peersrpc.UpdateAction_ADD,
			Address: newAddresses[0],
		},
		{
			Action:  peersrpc.UpdateAction_ADD,
			Address: newAddresses[1],
		},
		{
			Action:  peersrpc.UpdateAction_REMOVE,
			Address: defaultAddrs[0].Addr,
		},
	}

	updateFeatureActions := []*peersrpc.UpdateFeatureAction{
		{
			Action:     peersrpc.UpdateAction_ADD,
			FeatureBit: featureBit,
		},
	}

	nodeAnnReq := &peersrpc.NodeAnnouncementUpdateRequest{
		Alias:          newAlias,
		Color:          newColor,
		AddressUpdates: updateAddressActions,
		FeatureUpdates: updateFeatureActions,
	}

	response := ht.UpdateNodeAnnouncement(dave, nodeAnnReq)

	expectedOps := map[string]int{
		"features":  1,
		"color":     1,
		"alias":     1,
		"addresses": 3,
	}
	assertUpdateNodeAnnouncementResponse(ht, response, expectedOps)

	newNodeAddresses := []*lnrpc.NodeAddress{}
	// We removed the first address.
	newNodeAddresses = append(newNodeAddresses, defaultAddrs[1:]...)
	newNodeAddresses = append(
		newNodeAddresses,
		&lnrpc.NodeAddress{Addr: newAddresses[0], Network: "tcp"},
		&lnrpc.NodeAddress{Addr: newAddresses[1], Network: "tcp"},
	)

	// After updating the node we expect the update to contain
	// the requested color, requested alias and the new added addresses.
	newDaveNodeAnn := &lnrpc.NodeUpdate{
		Alias:         newAlias,
		Color:         newColor,
		NodeAddresses: newNodeAddresses,
	}

	// We'll then wait for Alice to receive dave's node announcement
	// with the new values.
	assertNodeAnn(newDaveNodeAnn)

	// Check that the feature bit was set correctly.
	resp = ht.GetInfo(dave)
	_, ok = resp.Features[featureIdx]
	require.True(ht, ok, "failed to set feature bit")

	// Check that we cannot set a feature bit that is already set.
	nodeAnnReq = &peersrpc.NodeAnnouncementUpdateRequest{
		FeatureUpdates: updateFeatureActions,
	}
	ht.UpdateNodeAnnouncementErr(dave, nodeAnnReq)

	// Check that we can unset feature bits.
	updateFeatureActions = []*peersrpc.UpdateFeatureAction{
		{
			Action:     peersrpc.UpdateAction_REMOVE,
			FeatureBit: featureBit,
		},
	}

	nodeAnnReq = &peersrpc.NodeAnnouncementUpdateRequest{
		FeatureUpdates: updateFeatureActions,
	}
	response = ht.UpdateNodeAnnouncement(dave, nodeAnnReq)

	expectedOps = map[string]int{
		"features": 1,
	}
	assertUpdateNodeAnnouncementResponse(ht, response, expectedOps)

	resp = ht.GetInfo(dave)
	_, ok = resp.Features[featureIdx]
	require.False(ht, ok, "failed to unset feature bit")

	// Check that we cannot unset a feature bit that is already unset.
	nodeAnnReq = &peersrpc.NodeAnnouncementUpdateRequest{
		FeatureUpdates: updateFeatureActions,
	}
	ht.UpdateNodeAnnouncementErr(dave, nodeAnnReq)

	// Close the channel between Bob and Dave.
	ht.CloseChannel(bob, chanPoint, false)
}

// assertSyncType asserts that the peer has an expected syncType.
//
// NOTE: only made for tests in this file.
func assertSyncType(ht *lntest.HarnessTest, hn *lntest.HarnessNode,
	peer string, syncType lnrpc.Peer_SyncType) {

	resp := ht.ListPeers(hn)
	for _, rpcPeer := range resp.Peers {
		if rpcPeer.PubKey != peer {
			continue
		}

		require.Equal(ht, syncType, rpcPeer.SyncType)
		return
	}

	ht.Fatalf("unable to find peer: %s", peer)
}

// assertUpdateNodeAnnouncementResponse is a helper function to assert
// the response expected values.
//
// NOTE: only used for tests in this file.
func assertUpdateNodeAnnouncementResponse(ht *lntest.HarnessTest,
	response *peersrpc.NodeAnnouncementUpdateResponse,
	expectedOps map[string]int) {

	require.Equal(
		ht, len(response.Ops), len(expectedOps),
		"unexpected number of Ops updating dave's node announcement",
	)

	ops := make(map[string]int, len(response.Ops))
	for _, op := range response.Ops {
		ops[op.Entity] = len(op.Actions)
	}

	for k, v := range expectedOps {
		if v != ops[k] {
			ht.Fatalf("unexpected number of actions for operation "+
				"%s: got %d wanted %d", k, ops[k], v)
		}
	}
}

// compareNodeAnns compares that two node announcements match or returns an
// error.
//
// NOTE: only used for tests in this file.
func compareNodeAnns(n1, n2 *lnrpc.NodeUpdate) error {
	// Alias should match.
	if n1.Alias != n2.Alias {
		return fmt.Errorf("alias not match")
	}

	// Color should match.
	if n1.Color != n2.Color {
		return fmt.Errorf("color not match")
	}

	// NodeAddresses should match.
	if len(n1.NodeAddresses) != len(n2.NodeAddresses) {
		return fmt.Errorf("node addresses don't match")
	}

	addrs := make(map[string]struct{}, len(n1.NodeAddresses))
	for _, nodeAddr := range n1.NodeAddresses {
		addrs[nodeAddr.Addr] = struct{}{}
	}

	for _, nodeAddr := range n2.NodeAddresses {
		if _, ok := addrs[nodeAddr.Addr]; !ok {
			return fmt.Errorf("address %v not found in node "+
				"announcement", nodeAddr.Addr)
		}
	}

	return nil
}
