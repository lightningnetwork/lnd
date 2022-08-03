package itest

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// openChannelStream blocks until an OpenChannel request for a channel funding
// by alice succeeds. If it does, a stream client is returned to receive events
// about the opening channel.
func openChannelStream(t *harnessTest, net *lntest.NetworkHarness,
	alice, bob *lntest.HarnessNode,
	p lntest.OpenChannelParams) lnrpc.Lightning_OpenChannelClient {

	t.t.Helper()

	// Wait until we are able to fund a channel successfully. This wait
	// prevents us from erroring out when trying to create a channel while
	// the node is starting up.
	var chanOpenUpdate lnrpc.Lightning_OpenChannelClient
	err := wait.NoError(func() error {
		var err error
		chanOpenUpdate, err = net.OpenChannel(alice, bob, p)
		return err
	}, defaultTimeout)
	require.NoError(t.t, err, "unable to open channel")

	return chanOpenUpdate
}

// openChannelAndAssert attempts to open a channel with the specified
// parameters extended from Alice to Bob. Additionally, two items are asserted
// after the channel is considered open: the funding transaction should be
// found within a block, and that Alice can report the status of the new
// channel.
func openChannelAndAssert(t *harnessTest, net *lntest.NetworkHarness,
	alice, bob *lntest.HarnessNode,
	p lntest.OpenChannelParams) *lnrpc.ChannelPoint {

	t.t.Helper()

	chanOpenUpdate := openChannelStream(t, net, alice, bob, p)

	// Mine 6 blocks, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the first newly mined block. We mine 6 blocks so that in the
	// case that the channel is public, it is announced to the network.
	block := mineBlocks(t, net, 6, 1)[0]

	fundingChanPoint, err := net.WaitForChannelOpen(chanOpenUpdate)
	require.NoError(t.t, err, "error while waiting for channel open")

	fundingTxID, err := lnrpc.GetChanPointFundingTxid(fundingChanPoint)
	require.NoError(t.t, err, "unable to get txid")

	assertTxInBlock(t, block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: fundingChanPoint.OutputIndex,
	}
	require.NoError(
		t.t, net.AssertChannelExists(alice, &chanPoint),
		"unable to assert channel existence",
	)
	require.NoError(
		t.t, net.AssertChannelExists(bob, &chanPoint),
		"unable to assert channel existence",
	)

	// They should also notice this channel from topology subscription.
	err = alice.WaitForNetworkChannelOpen(fundingChanPoint)
	require.NoError(t.t, err)

	err = bob.WaitForNetworkChannelOpen(fundingChanPoint)
	require.NoError(t.t, err)

	return fundingChanPoint
}

func waitForGraphSync(t *harnessTest, node *lntest.HarnessNode) {
	t.t.Helper()

	err := wait.Predicate(func() bool {
		ctxb := context.Background()
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		resp, err := node.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
		require.NoError(t.t, err)

		return resp.SyncedToGraph
	}, defaultTimeout)
	require.NoError(t.t, err)
}

// closeChannelAndAssert attempts to close a channel identified by the passed
// channel point owned by the passed Lightning node. A fully blocking channel
// closure is attempted, therefore the passed context should be a child derived
// via timeout from a base parent. Additionally, once the channel has been
// detected as closed, an assertion checks that the transaction is found within
// a block. Finally, this assertion verifies that the node always sends out a
// disable update when closing the channel if the channel was previously
// enabled.
//
// NOTE: This method assumes that the provided funding point is confirmed
// on-chain AND that the edge exists in the node's channel graph. If the funding
// transactions was reorged out at some point, use closeReorgedChannelAndAssert.
func closeChannelAndAssert(t *harnessTest, net *lntest.NetworkHarness,
	node *lntest.HarnessNode, fundingChanPoint *lnrpc.ChannelPoint,
	force bool) *chainhash.Hash {

	return closeChannelAndAssertType(
		t, net, node, fundingChanPoint, false, force,
	)
}

func closeChannelAndAssertType(t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint,
	anchors, force bool) *chainhash.Hash {

	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, channelCloseTimeout)
	defer cancel()

	// Fetch the current channel policy. If the channel is currently
	// enabled, we will register for graph notifications before closing to
	// assert that the node sends out a disabling update as a result of the
	// channel being closed.
	curPolicy := getChannelPolicies(
		t, node, node.PubKeyStr, fundingChanPoint,
	)[0]
	expectDisable := !curPolicy.Disabled

	closeUpdates, _, err := net.CloseChannel(node, fundingChanPoint, force)
	require.NoError(t.t, err, "unable to close channel")

	// If the channel policy was enabled prior to the closure, wait until we
	// received the disabled update.
	if expectDisable {
		curPolicy.Disabled = true
		assertChannelPolicyUpdate(
			t.t, node, node.PubKeyStr,
			curPolicy, fundingChanPoint, false,
		)
	}

	return assertChannelClosed(
		ctxt, t, net, node, fundingChanPoint, anchors, closeUpdates,
	)
}

// closeReorgedChannelAndAssert attempts to close a channel identified by the
// passed channel point owned by the passed Lightning node. A fully blocking
// channel closure is attempted, therefore the passed context should be a child
// derived via timeout from a base parent. Additionally, once the channel has
// been detected as closed, an assertion checks that the transaction is found
// within a block.
//
// NOTE: This method does not verify that the node sends a disable update for
// the closed channel.
func closeReorgedChannelAndAssert(t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	ctxb := context.Background()
	ctx, cancel := context.WithTimeout(ctxb, channelCloseTimeout)
	defer cancel()

	closeUpdates, _, err := net.CloseChannel(node, fundingChanPoint, force)
	require.NoError(t.t, err, "unable to close channel")

	return assertChannelClosed(
		ctx, t, net, node, fundingChanPoint, false, closeUpdates,
	)
}

// assertChannelClosed asserts that the channel is properly cleaned up after
// initiating a cooperative or local close.
func assertChannelClosed(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, anchors bool,
	closeUpdates lnrpc.Lightning_CloseChannelClient) *chainhash.Hash {

	txid, err := lnrpc.GetChanPointFundingTxid(fundingChanPoint)
	require.NoError(t.t, err, "unable to get txid")
	chanPointStr := fmt.Sprintf("%v:%v", txid, fundingChanPoint.OutputIndex)

	// If the channel appears in list channels, ensure that its state
	// contains ChanStatusCoopBroadcasted.
	listChansRequest := &lnrpc.ListChannelsRequest{}
	listChansResp, err := node.ListChannels(ctx, listChansRequest)
	require.NoError(t.t, err, "unable to query for list channels")

	for _, channel := range listChansResp.Channels {
		// Skip other channels.
		if channel.ChannelPoint != chanPointStr {
			continue
		}

		// Assert that the channel is in coop broadcasted.
		require.Contains(
			t.t, channel.ChanStatusFlags,
			channeldb.ChanStatusCoopBroadcasted.String(),
			"channel not coop broadcasted",
		)
	}

	// At this point, the channel should now be marked as being in the
	// state of "waiting close".
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	pendingChanResp, err := node.PendingChannels(ctx, pendingChansRequest)
	require.NoError(t.t, err, "unable to query for pending channels")

	var found bool
	for _, pendingClose := range pendingChanResp.WaitingCloseChannels {
		if pendingClose.Channel.ChannelPoint == chanPointStr {
			found = true
			break
		}
	}
	require.True(t.t, found, "channel not marked as waiting close")

	// We'll now, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block. If there are anchors, we also expect an anchor sweep.
	expectedTxes := 1
	if anchors {
		expectedTxes = 2
	}

	block := mineBlocks(t, net, 1, expectedTxes)[0]

	closingTxid, err := net.WaitForChannelClose(closeUpdates)
	require.NoError(t.t, err, "error while waiting for channel close")

	assertTxInBlock(t, block, closingTxid)

	// Finally, the transaction should no longer be in the waiting close
	// state as we've just mined a block that should include the closing
	// transaction.
	err = wait.Predicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := node.PendingChannels(
			ctx, pendingChansRequest,
		)
		if err != nil {
			return false
		}

		for _, pendingClose := range pendingChanResp.WaitingCloseChannels {
			if pendingClose.Channel.ChannelPoint == chanPointStr {
				return false
			}
		}

		return true
	}, defaultTimeout)
	require.NoError(
		t.t, err, "closing transaction not marked as fully closed",
	)

	return closingTxid
}

// findForceClosedChannel searches a pending channel response for a particular
// channel, returning the force closed channel upon success.
func findForceClosedChannel(pendingChanResp *lnrpc.PendingChannelsResponse,
	op fmt.Stringer) (*lnrpc.PendingChannelsResponse_ForceClosedChannel,
	error) {

	for _, forceClose := range pendingChanResp.PendingForceClosingChannels {
		if forceClose.Channel.ChannelPoint == op.String() {
			return forceClose, nil
		}
	}

	return nil, errors.New("channel not marked as force closed")
}

// findWaitingCloseChannel searches a pending channel response for a particular
// channel, returning the waiting close channel upon success.
func findWaitingCloseChannel(pendingChanResp *lnrpc.PendingChannelsResponse,
	op fmt.Stringer) (*lnrpc.PendingChannelsResponse_WaitingCloseChannel,
	error) {

	for _, waitingClose := range pendingChanResp.WaitingCloseChannels {
		if waitingClose.Channel.ChannelPoint == op.String() {
			return waitingClose, nil
		}
	}

	return nil, errors.New("channel not marked as waiting close")
}

// waitForChannelPendingForceClose waits for the node to report that the
// channel is pending force close, and that the UTXO nursery is aware of it.
func waitForChannelPendingForceClose(node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint) error {

	ctxb := context.Background()
	ctx, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	txid, err := lnrpc.GetChanPointFundingTxid(fundingChanPoint)
	if err != nil {
		return err
	}

	op := wire.OutPoint{
		Hash:  *txid,
		Index: fundingChanPoint.OutputIndex,
	}

	return wait.NoError(func() error {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := node.PendingChannels(
			ctx, pendingChansRequest,
		)
		if err != nil {
			return fmt.Errorf("unable to get pending channels: %v",
				err)
		}

		forceClose, err := findForceClosedChannel(pendingChanResp, &op)
		if err != nil {
			return err
		}

		// We must wait until the UTXO nursery has received the channel
		// and is aware of its maturity height.
		if forceClose.MaturityHeight == 0 {
			return fmt.Errorf("channel had maturity height of 0")
		}

		return nil
	}, defaultTimeout)
}

// lnrpcForceCloseChannel is a short type alias for a ridiculously long type
// name in the lnrpc package.
type lnrpcForceCloseChannel = lnrpc.PendingChannelsResponse_ForceClosedChannel

// waitForNumChannelPendingForceClose waits for the node to report a certain
// number of channels in state pending force close.
func waitForNumChannelPendingForceClose(node *lntest.HarnessNode,
	expectedNum int,
	perChanCheck func(channel *lnrpcForceCloseChannel) error) error {

	ctxb := context.Background()
	ctx, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	return wait.NoError(func() error {
		resp, err := node.PendingChannels(
			ctx, &lnrpc.PendingChannelsRequest{},
		)
		if err != nil {
			return fmt.Errorf("unable to get pending channels: %v",
				err)
		}

		forceCloseChans := resp.PendingForceClosingChannels
		if len(forceCloseChans) != expectedNum {
			return fmt.Errorf("%v should have %d pending "+
				"force close channels but has %d",
				node.Cfg.Name, expectedNum,
				len(forceCloseChans))
		}

		if perChanCheck != nil {
			for _, forceCloseChan := range forceCloseChans {
				err := perChanCheck(forceCloseChan)
				if err != nil {
					return err
				}
			}
		}

		return nil
	}, defaultTimeout)
}

// cleanupForceClose mines a force close commitment found in the mempool and
// the following sweep transaction from the force closing node.
func cleanupForceClose(t *harnessTest, net *lntest.NetworkHarness,
	node *lntest.HarnessNode, chanPoint *lnrpc.ChannelPoint) {

	// Wait for the channel to be marked pending force close.
	err := waitForChannelPendingForceClose(node, chanPoint)
	require.NoError(t.t, err, "channel not pending force close")

	// Mine enough blocks for the node to sweep its funds from the force
	// closed channel.
	//
	// The commit sweep resolver is able to broadcast the sweep tx up to
	// one block before the CSV elapses, so wait until defaulCSV-1.
	_, err = net.Miner.Client.Generate(defaultCSV - 1)
	require.NoError(t.t, err, "unable to generate blocks")

	// The node should now sweep the funds, clean up by mining the sweeping
	// tx.
	mineBlocks(t, net, 1, 1)
}

// numOpenChannelsPending sends an RPC request to a node to get a count of the
// node's channels that are currently in a pending state (with a broadcast, but
// not confirmed funding transaction).
func numOpenChannelsPending(ctxt context.Context,
	node *lntest.HarnessNode) (int, error) {

	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	resp, err := node.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		return 0, err
	}
	return len(resp.PendingOpenChannels), nil
}

// assertNumOpenChannelsPending asserts that a pair of nodes have the expected
// number of pending channels between them.
func assertNumOpenChannelsPending(t *harnessTest,
	alice, bob *lntest.HarnessNode, expected int) {

	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	err := wait.NoError(func() error {
		aliceNumChans, err := numOpenChannelsPending(ctxt, alice)
		if err != nil {
			return fmt.Errorf("error fetching alice's node (%v) "+
				"pending channels %v", alice.NodeID, err)
		}
		bobNumChans, err := numOpenChannelsPending(ctxt, bob)
		if err != nil {
			return fmt.Errorf("error fetching bob's node (%v) "+
				"pending channels %v", bob.NodeID, err)
		}

		aliceStateCorrect := aliceNumChans == expected
		if !aliceStateCorrect {
			return fmt.Errorf("number of pending channels for "+
				"alice incorrect. expected %v, got %v",
				expected, aliceNumChans)
		}

		bobStateCorrect := bobNumChans == expected
		if !bobStateCorrect {
			return fmt.Errorf("number of pending channels for bob "+
				"incorrect. expected %v, got %v", expected,
				bobNumChans)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t.t, err)
}

// checkPeerInPeersList returns true if Bob appears in Alice's peer list.
func checkPeerInPeersList(ctx context.Context, alice,
	bob *lntest.HarnessNode) (bool, error) {

	peers, err := alice.ListPeers(ctx, &lnrpc.ListPeersRequest{})
	if err != nil {
		return false, fmt.Errorf(
			"error listing %s's node (%v) peers: %v",
			alice.Name(), alice.NodeID, err,
		)
	}

	for _, peer := range peers.Peers {
		if peer.PubKey == bob.PubKeyStr {
			return true, nil
		}
	}

	return false, nil
}

// assertConnected asserts that two peers are connected.
func assertConnected(t *harnessTest, alice, bob *lntest.HarnessNode) {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	err := wait.NoError(func() error {
		bobIsAlicePeer, err := checkPeerInPeersList(ctxt, alice, bob)
		if err != nil {
			return err
		}

		if !bobIsAlicePeer {
			return fmt.Errorf(
				"expected %s and %s to be connected "+
					"but %s is not in %s's peer list",
				alice.Name(), bob.Name(),
				bob.Name(), alice.Name(),
			)
		}

		aliceIsBobPeer, err := checkPeerInPeersList(ctxt, bob, alice)
		if err != nil {
			return err
		}

		if !aliceIsBobPeer {
			return fmt.Errorf(
				"expected %s and %s to be connected "+
					"but %s is not in %s's peer list",
				alice.Name(), bob.Name(),
				alice.Name(), bob.Name(),
			)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t.t, err)
}

// assertNotConnected asserts that two peers are not connected.
func assertNotConnected(t *harnessTest, alice, bob *lntest.HarnessNode) {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	err := wait.NoError(func() error {
		bobIsAlicePeer, err := checkPeerInPeersList(ctxt, alice, bob)
		if err != nil {
			return err
		}

		if bobIsAlicePeer {
			return fmt.Errorf(
				"expected %s and %s not to be "+
					"connected but %s is in %s's "+
					"peer list",
				alice.Name(), bob.Name(),
				bob.Name(), alice.Name(),
			)
		}

		aliceIsBobPeer, err := checkPeerInPeersList(ctxt, bob, alice)
		if err != nil {
			return err
		}

		if aliceIsBobPeer {
			return fmt.Errorf(
				"expected %s and %s not to be "+
					"connected but %s is in %s's "+
					"peer list",
				alice.Name(), bob.Name(),
				alice.Name(), bob.Name(),
			)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t.t, err)
}

// shutdownAndAssert shuts down the given node and asserts that no errors
// occur.
func shutdownAndAssert(net *lntest.NetworkHarness, t *harnessTest,
	node *lntest.HarnessNode) {

	// The process may not be in a state to always shutdown immediately, so
	// we'll retry up to a hard limit to ensure we eventually shutdown.
	err := wait.NoError(func() error {
		return net.ShutdownNode(node)
	}, defaultTimeout)
	require.NoErrorf(t.t, err, "unable to shutdown %v", node.Name())
}

// assertChannelBalanceResp makes a ChannelBalance request and checks the
// returned response matches the expected.
func assertChannelBalanceResp(t *harnessTest, node *lntest.HarnessNode,
	expected *lnrpc.ChannelBalanceResponse) {

	resp := getChannelBalance(t, node)
	require.True(t.t, proto.Equal(expected, resp), "balance is incorrect")
}

// getChannelBalance gets the channel balance.
func getChannelBalance(t *harnessTest,
	node *lntest.HarnessNode) *lnrpc.ChannelBalanceResponse {

	t.t.Helper()

	ctxt, _ := context.WithTimeout(context.Background(), defaultTimeout)
	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := node.ChannelBalance(ctxt, req)

	require.NoError(t.t, err, "unable to get node's balance")
	return resp
}

// txStr returns the string representation of the channel's funding transaction.
func txStr(chanPoint *lnrpc.ChannelPoint) string {
	fundingTxID, err := lnrpc.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		return ""
	}
	cp := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPoint.OutputIndex,
	}
	return cp.String()
}

// getChannelPolicies queries the channel graph and retrieves the current edge
// policies for the provided channel points.
func getChannelPolicies(t *harnessTest, node *lntest.HarnessNode,
	advertisingNode string,
	chanPoints ...*lnrpc.ChannelPoint) []*lnrpc.RoutingPolicy {

	ctxb := context.Background()

	descReq := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	chanGraph, err := node.DescribeGraph(ctxt, descReq)
	require.NoError(t.t, err, "unable to query for alice's graph")

	var policies []*lnrpc.RoutingPolicy
	err = wait.NoError(func() error {
	out:
		for _, chanPoint := range chanPoints {
			for _, e := range chanGraph.Edges {
				if e.ChanPoint != txStr(chanPoint) {
					continue
				}

				if e.Node1Pub == advertisingNode {
					policies = append(policies,
						e.Node1Policy)
				} else {
					policies = append(policies,
						e.Node2Policy)
				}

				continue out
			}

			// If we've iterated over all the known edges and we weren't
			// able to find this specific one, then we'll fail.
			return fmt.Errorf("did not find edge %v", txStr(chanPoint))
		}

		return nil
	}, defaultTimeout)
	require.NoError(t.t, err)

	return policies
}

// assertChannelPolicy asserts that the passed node's known channel policy for
// the passed chanPoint is consistent with the expected policy values.
func assertChannelPolicy(t *harnessTest, node *lntest.HarnessNode,
	advertisingNode string, expectedPolicy *lnrpc.RoutingPolicy,
	chanPoints ...*lnrpc.ChannelPoint) {

	policies := getChannelPolicies(t, node, advertisingNode, chanPoints...)
	for _, policy := range policies {
		err := lntest.CheckChannelPolicy(policy, expectedPolicy)
		if err != nil {
			t.Fatalf(fmt.Sprintf("%v: %s", err.Error(), node))
		}
	}
}

// assertMinerBlockHeightDelta ensures that tempMiner is 'delta' blocks ahead
// of miner.
func assertMinerBlockHeightDelta(t *harnessTest,
	miner, tempMiner *lntest.HarnessMiner, delta int32) {

	// Ensure the chain lengths are what we expect.
	var predErr error
	err := wait.Predicate(func() bool {
		_, tempMinerHeight, err := tempMiner.Client.GetBestBlock()
		if err != nil {
			predErr = fmt.Errorf("unable to get current "+
				"blockheight %v", err)
			return false
		}

		_, minerHeight, err := miner.Client.GetBestBlock()
		if err != nil {
			predErr = fmt.Errorf("unable to get current "+
				"blockheight %v", err)
			return false
		}

		if tempMinerHeight != minerHeight+delta {
			predErr = fmt.Errorf("expected new miner(%d) to be %d "+
				"blocks ahead of original miner(%d)",
				tempMinerHeight, delta, minerHeight)
			return false
		}
		return true
	}, defaultTimeout)
	if err != nil {
		t.Fatalf(predErr.Error())
	}
}

func checkCommitmentMaturity(
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	maturityHeight uint32, blocksTilMaturity int32) error {

	if forceClose.MaturityHeight != maturityHeight {
		return fmt.Errorf("expected commitment maturity height to be "+
			"%d, found %d instead", maturityHeight,
			forceClose.MaturityHeight)
	}
	if forceClose.BlocksTilMaturity != blocksTilMaturity {
		return fmt.Errorf("expected commitment blocks til maturity to "+
			"be %d, found %d instead", blocksTilMaturity,
			forceClose.BlocksTilMaturity)
	}

	return nil
}

// checkForceClosedChannelNumHtlcs verifies that a force closed channel has the
// proper number of htlcs.
func checkPendingChannelNumHtlcs(
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	expectedNumHtlcs int) error {

	if len(forceClose.PendingHtlcs) != expectedNumHtlcs {
		return fmt.Errorf("expected force closed channel to have %d "+
			"pending htlcs, found %d instead", expectedNumHtlcs,
			len(forceClose.PendingHtlcs))
	}

	return nil
}

// checkNumForceClosedChannels checks that a pending channel response has the
// expected number of force closed channels.
func checkNumForceClosedChannels(pendingChanResp *lnrpc.PendingChannelsResponse,
	expectedNumChans int) error {

	if len(pendingChanResp.PendingForceClosingChannels) != expectedNumChans {
		return fmt.Errorf("expected to find %d force closed channels, "+
			"got %d", expectedNumChans,
			len(pendingChanResp.PendingForceClosingChannels))
	}

	return nil
}

// checkNumWaitingCloseChannels checks that a pending channel response has the
// expected number of channels waiting for closing tx to confirm.
func checkNumWaitingCloseChannels(pendingChanResp *lnrpc.PendingChannelsResponse,
	expectedNumChans int) error {

	if len(pendingChanResp.WaitingCloseChannels) != expectedNumChans {
		return fmt.Errorf("expected to find %d channels waiting "+
			"closure, got %d", expectedNumChans,
			len(pendingChanResp.WaitingCloseChannels))
	}

	return nil
}

// checkPendingHtlcStageAndMaturity uniformly tests all pending htlc's belonging
// to a force closed channel, testing for the expected stage number, blocks till
// maturity, and the maturity height.
func checkPendingHtlcStageAndMaturity(
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	stage, maturityHeight uint32, blocksTillMaturity int32) error {

	for _, pendingHtlc := range forceClose.PendingHtlcs {
		if pendingHtlc.Stage != stage {
			return fmt.Errorf("expected pending htlc to be stage "+
				"%d, found %d", stage, pendingHtlc.Stage)
		}
		if pendingHtlc.MaturityHeight != maturityHeight {
			return fmt.Errorf("expected pending htlc maturity "+
				"height to be %d, instead has %d",
				maturityHeight, pendingHtlc.MaturityHeight)
		}
		if pendingHtlc.BlocksTilMaturity != blocksTillMaturity {
			return fmt.Errorf("expected pending htlc blocks til "+
				"maturity to be %d, instead has %d",
				blocksTillMaturity,
				pendingHtlc.BlocksTilMaturity)
		}
	}

	return nil
}

// assertReports checks that the count of resolutions we have present per
// type matches a set of expected resolutions.
func assertReports(t *harnessTest, node *lntest.HarnessNode,
	channelPoint wire.OutPoint, expected map[string]*lnrpc.Resolution) {

	// Get our node's closed channels.
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	closed, err := node.ClosedChannels(
		ctxt, &lnrpc.ClosedChannelsRequest{},
	)
	require.NoError(t.t, err)

	var resolutions []*lnrpc.Resolution
	for _, close := range closed.Channels {
		if close.ChannelPoint == channelPoint.String() {
			resolutions = close.Resolutions
			break
		}
	}

	require.NotNil(t.t, resolutions)
	require.Equal(t.t, len(expected), len(resolutions))

	for _, res := range resolutions {
		outPointStr := fmt.Sprintf("%v:%v", res.Outpoint.TxidStr,
			res.Outpoint.OutputIndex)

		expected, ok := expected[outPointStr]
		require.True(t.t, ok)
		require.Equal(t.t, expected, res)
	}
}

// assertSweepFound looks up a sweep in a nodes list of broadcast sweeps.
func assertSweepFound(t *testing.T, node *lntest.HarnessNode,
	sweep string, verbose bool) {

	// List all sweeps that alice's node had broadcast.
	ctxb := context.Background()
	ctx, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	sweepResp, err := node.WalletKitClient.ListSweeps(
		ctx, &walletrpc.ListSweepsRequest{
			Verbose: verbose,
		},
	)
	require.NoError(t, err)

	var found bool
	if verbose {
		found = findSweepInDetails(t, sweep, sweepResp)
	} else {
		found = findSweepInTxids(t, sweep, sweepResp)
	}

	require.True(t, found, "sweep: %v not found", sweep)
}

func findSweepInTxids(t *testing.T, sweepTxid string,
	sweepResp *walletrpc.ListSweepsResponse) bool {

	sweepTxIDs := sweepResp.GetTransactionIds()
	require.NotNil(t, sweepTxIDs, "expected transaction ids")
	require.Nil(t, sweepResp.GetTransactionDetails())

	// Check that the sweep tx we have just produced is present.
	for _, tx := range sweepTxIDs.TransactionIds {
		if tx == sweepTxid {
			return true
		}
	}

	return false
}

func findSweepInDetails(t *testing.T, sweepTxid string,
	sweepResp *walletrpc.ListSweepsResponse) bool {

	sweepDetails := sweepResp.GetTransactionDetails()
	require.NotNil(t, sweepDetails, "expected transaction details")
	require.Nil(t, sweepResp.GetTransactionIds())

	for _, tx := range sweepDetails.Transactions {
		if tx.TxHash == sweepTxid {
			return true
		}
	}

	return false
}

// assertAmountSent generates a closure which queries listchannels for sndr and
// rcvr, and asserts that sndr sent amt satoshis, and that rcvr received amt
// satoshis.
//
// NOTE: This method assumes that each node only has one channel, and it is the
// channel used to send the payment.
func assertAmountSent(amt btcutil.Amount, sndr, rcvr *lntest.HarnessNode) func() error {
	return func() error {
		// Both channels should also have properly accounted from the
		// amount that has been sent/received over the channel.
		listReq := &lnrpc.ListChannelsRequest{}
		ctxb := context.Background()
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		sndrListChannels, err := sndr.ListChannels(ctxt, listReq)
		if err != nil {
			return fmt.Errorf("unable to query for %s's channel "+
				"list: %v", sndr.Name(), err)
		}
		sndrSatoshisSent := sndrListChannels.Channels[0].TotalSatoshisSent
		if sndrSatoshisSent != int64(amt) {
			return fmt.Errorf("%s's satoshis sent is incorrect "+
				"got %v, expected %v", sndr.Name(),
				sndrSatoshisSent, amt)
		}

		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		rcvrListChannels, err := rcvr.ListChannels(ctxt, listReq)
		if err != nil {
			return fmt.Errorf("unable to query for %s's channel "+
				"list: %v", rcvr.Name(), err)
		}
		rcvrSatoshisReceived := rcvrListChannels.Channels[0].TotalSatoshisReceived
		if rcvrSatoshisReceived != int64(amt) {
			return fmt.Errorf("%s's satoshis received is "+
				"incorrect got %v, expected %v", rcvr.Name(),
				rcvrSatoshisReceived, amt)
		}

		return nil
	}
}

// assertLastHTLCError checks that the last sent HTLC of the last payment sent
// by the given node failed with the expected failure code.
func assertLastHTLCError(t *harnessTest, node *lntest.HarnessNode,
	code lnrpc.Failure_FailureCode) {

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
	}
	ctxt, _ := context.WithTimeout(context.Background(), defaultTimeout)
	paymentsResp, err := node.ListPayments(ctxt, req)
	require.NoError(t.t, err, "error when obtaining payments")

	payments := paymentsResp.Payments
	require.NotZero(t.t, len(payments), "no payments found")

	payment := payments[len(payments)-1]
	htlcs := payment.Htlcs
	require.NotZero(t.t, len(htlcs), "no htlcs")

	htlc := htlcs[len(htlcs)-1]
	require.NotNil(t.t, htlc.Failure, "expected failure")

	require.Equal(t.t, code, htlc.Failure.Code, "unexpected failure code")
}

// assertAmountPaid checks that the ListChannels command of the provided
// node list the total amount sent and received as expected for the
// provided channel.
func assertAmountPaid(t *harnessTest, channelName string,
	node *lntest.HarnessNode, chanPoint wire.OutPoint, amountSent,
	amountReceived int64) {

	ctxb := context.Background()

	checkAmountPaid := func() error {
		listReq := &lnrpc.ListChannelsRequest{}
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		resp, err := node.ListChannels(ctxt, listReq)
		if err != nil {
			return fmt.Errorf("unable to for node's "+
				"channels: %v", err)
		}
		for _, channel := range resp.Channels {
			if channel.ChannelPoint != chanPoint.String() {
				continue
			}

			if channel.TotalSatoshisSent != amountSent {
				return fmt.Errorf("%v: incorrect amount"+
					" sent: %v != %v", channelName,
					channel.TotalSatoshisSent,
					amountSent)
			}
			if channel.TotalSatoshisReceived !=
				amountReceived {

				return fmt.Errorf("%v: incorrect amount"+
					" received: %v != %v",
					channelName,
					channel.TotalSatoshisReceived,
					amountReceived)
			}

			return nil
		}
		return fmt.Errorf("channel not found")
	}

	// As far as HTLC inclusion in commitment transaction might be
	// postponed we will try to check the balance couple of times,
	// and then if after some period of time we receive wrong
	// balance return the error.
	// TODO(roasbeef): remove sleep after invoice notification hooks
	// are in place
	var timeover uint32
	go func() {
		<-time.After(defaultTimeout)
		atomic.StoreUint32(&timeover, 1)
	}()

	for {
		isTimeover := atomic.LoadUint32(&timeover) == 1
		if err := checkAmountPaid(); err != nil {
			require.Falsef(
				t.t, isTimeover,
				"Check amount Paid failed: %v", err,
			)
		} else {
			break
		}
	}
}

// assertNumPendingChannels checks that a PendingChannels response from the
// node reports the expected number of pending channels.
func assertNumPendingChannels(t *harnessTest, node *lntest.HarnessNode,
	expWaitingClose, expPendingForceClose int) {

	ctxb := context.Background()

	var predErr error
	err := wait.Predicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := node.PendingChannels(ctxt,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.WaitingCloseChannels)
		if n != expWaitingClose {
			predErr = fmt.Errorf("expected to find %d channels "+
				"waiting close, found %d", expWaitingClose, n)
			return false
		}
		n = len(pendingChanResp.PendingForceClosingChannels)
		if n != expPendingForceClose {
			predErr = fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d", expPendingForceClose, n)
			return false
		}
		return true
	}, defaultTimeout)
	require.NoErrorf(t.t, err, "got err: %v", predErr)
}

// verifyCloseUpdate is used to verify that a closed channel update is of the
// expected type.
func verifyCloseUpdate(chanUpdate *lnrpc.ChannelEventUpdate,
	closeType lnrpc.ChannelCloseSummary_ClosureType,
	closeInitiator lnrpc.Initiator) error {

	// We should receive one inactive and one closed notification
	// for each channel.
	switch update := chanUpdate.Channel.(type) {
	case *lnrpc.ChannelEventUpdate_InactiveChannel:
		if chanUpdate.Type != lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL {
			return fmt.Errorf("update type mismatch: expected %v, got %v",
				lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL,
				chanUpdate.Type)
		}

	case *lnrpc.ChannelEventUpdate_ClosedChannel:
		if chanUpdate.Type !=
			lnrpc.ChannelEventUpdate_CLOSED_CHANNEL {

			return fmt.Errorf("update type mismatch: expected %v, got %v",
				lnrpc.ChannelEventUpdate_CLOSED_CHANNEL,
				chanUpdate.Type)
		}

		if update.ClosedChannel.CloseType != closeType {
			return fmt.Errorf("channel closure type "+
				"mismatch: expected %v, got %v",
				closeType,
				update.ClosedChannel.CloseType)
		}

		if update.ClosedChannel.CloseInitiator != closeInitiator {
			return fmt.Errorf("expected close intiator: %v, got: %v",
				closeInitiator,
				update.ClosedChannel.CloseInitiator)
		}

	case *lnrpc.ChannelEventUpdate_FullyResolvedChannel:
		if chanUpdate.Type != lnrpc.ChannelEventUpdate_FULLY_RESOLVED_CHANNEL {
			return fmt.Errorf("update type mismatch: expected %v, got %v",
				lnrpc.ChannelEventUpdate_FULLY_RESOLVED_CHANNEL,
				chanUpdate.Type)
		}

	default:
		return fmt.Errorf("channel update channel of wrong type, "+
			"expected closed channel, got %T",
			update)
	}

	return nil
}

// assertNodeNumChannels polls the provided node's list channels rpc until it
// reaches the desired number of total channels.
func assertNodeNumChannels(t *harnessTest, node *lntest.HarnessNode,
	numChannels int) {

	ctxb := context.Background()

	// Poll node for its list of channels.
	req := &lnrpc.ListChannelsRequest{}

	var predErr error
	pred := func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		chanInfo, err := node.ListChannels(ctxt, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query for node's "+
				"channels: %v", err)
			return false
		}

		// Return true if the query returned the expected number of
		// channels.
		num := len(chanInfo.Channels)
		if num != numChannels {
			predErr = fmt.Errorf("expected %v channels, got %v",
				numChannels, num)
			return false
		}
		return true
	}

	require.NoErrorf(
		t.t, wait.Predicate(pred, defaultTimeout),
		"node has incorrect number of channels: %v", predErr,
	)
}

// assertActiveHtlcs makes sure all the passed nodes have the _exact_ HTLCs
// matching payHashes on _all_ their channels.
func assertActiveHtlcs(nodes []*lntest.HarnessNode, payHashes ...[]byte) error {
	ctxb := context.Background()

	req := &lnrpc.ListChannelsRequest{}
	for _, node := range nodes {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		nodeChans, err := node.ListChannels(ctxt, req)
		if err != nil {
			return fmt.Errorf("unable to get node chans: %v", err)
		}

		for _, channel := range nodeChans.Channels {
			// Record all payment hashes active for this channel.
			htlcHashes := make(map[string]struct{})
			for _, htlc := range channel.PendingHtlcs {
				h := hex.EncodeToString(htlc.HashLock)
				_, ok := htlcHashes[h]
				if ok {
					return fmt.Errorf("duplicate HashLock")
				}
				htlcHashes[h] = struct{}{}
			}

			// Channel should have exactly the payHashes active.
			if len(payHashes) != len(htlcHashes) {
				return fmt.Errorf("node [%s:%x] had %v "+
					"htlcs active, expected %v",
					node.Cfg.Name, node.PubKey[:],
					len(htlcHashes), len(payHashes))
			}

			// Make sure all the payHashes are active.
			for _, payHash := range payHashes {
				h := hex.EncodeToString(payHash)
				if _, ok := htlcHashes[h]; ok {
					continue
				}
				return fmt.Errorf("node [%s:%x] didn't have: "+
					"the payHash %v active", node.Cfg.Name,
					node.PubKey[:], h)
			}
		}
	}

	return nil
}

func assertNumActiveHtlcsChanPoint(node *lntest.HarnessNode,
	chanPoint wire.OutPoint, numHtlcs int) error {

	ctxb := context.Background()

	req := &lnrpc.ListChannelsRequest{}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	nodeChans, err := node.ListChannels(ctxt, req)
	if err != nil {
		return err
	}

	for _, channel := range nodeChans.Channels {
		if channel.ChannelPoint != chanPoint.String() {
			continue
		}

		if len(channel.PendingHtlcs) != numHtlcs {
			return fmt.Errorf("expected %v active HTLCs, got %v",
				numHtlcs, len(channel.PendingHtlcs))
		}
		return nil
	}

	return fmt.Errorf("channel point %v not found", chanPoint)
}

func assertNumActiveHtlcs(nodes []*lntest.HarnessNode, numHtlcs int) error {
	ctxb := context.Background()

	req := &lnrpc.ListChannelsRequest{}
	for _, node := range nodes {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		nodeChans, err := node.ListChannels(ctxt, req)
		if err != nil {
			return err
		}

		for _, channel := range nodeChans.Channels {
			if len(channel.PendingHtlcs) != numHtlcs {
				return fmt.Errorf("expected %v HTLCs, got %v",
					numHtlcs, len(channel.PendingHtlcs))
			}
		}
	}

	return nil
}

func assertSpendingTxInMempool(t *harnessTest, miner *rpcclient.Client,
	timeout time.Duration, inputs ...wire.OutPoint) chainhash.Hash {

	tx := getSpendingTxInMempool(t, miner, timeout, inputs...)
	return tx.TxHash()
}

// getSpendingTxInMempool waits for a transaction spending the given outpoint to
// appear in the mempool and returns that tx in full.
func getSpendingTxInMempool(t *harnessTest, miner *rpcclient.Client,
	timeout time.Duration, inputs ...wire.OutPoint) *wire.MsgTx {

	inputSet := make(map[wire.OutPoint]struct{}, len(inputs))
	breakTimeout := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-breakTimeout:
			t.Fatalf("didn't find tx in mempool")
		case <-ticker.C:
			mempool, err := miner.GetRawMempool()
			require.NoError(t.t, err, "unable to get mempool")

			if len(mempool) == 0 {
				continue
			}

			for _, txid := range mempool {
				tx, err := miner.GetRawTransaction(txid)
				require.NoError(t.t, err, "unable to fetch tx")
				msgTx := tx.MsgTx()

				// Include the inputs again in case they were
				// removed in a previous iteration.
				for _, input := range inputs {
					inputSet[input] = struct{}{}
				}

				for _, txIn := range msgTx.TxIn {
					input := txIn.PreviousOutPoint
					delete(inputSet, input)
				}

				if len(inputSet) > 0 {
					// Missing input, check next transaction
					// or try again.
					continue
				}

				// Transaction spends all expected inputs,
				// return.
				return msgTx
			}
		}
	}
}

// assertTxLabel is a helper function which finds a target tx in our set
// of transactions and checks that it has the desired label.
func assertTxLabel(t *harnessTest, node *lntest.HarnessNode,
	targetTx, label string) {

	// List all transactions relevant to our wallet, and find the tx so that
	// we can check the correct label has been set.
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	txResp, err := node.GetTransactions(
		ctxt, &lnrpc.GetTransactionsRequest{},
	)
	require.NoError(t.t, err, "could not get transactions")

	// Find our transaction in the set of transactions returned and check
	// its label.
	for _, txn := range txResp.Transactions {
		if txn.TxHash == targetTx {
			require.Equal(t.t, label, txn.Label, "labels not match")
		}
	}
}

// sendAndAssertSuccess sends the given payment requests and asserts that the
// payment completes successfully.
func sendAndAssertSuccess(t *harnessTest, node *lntest.HarnessNode,
	req *routerrpc.SendPaymentRequest) *lnrpc.Payment {

	ctxb := context.Background()
	ctx, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	var result *lnrpc.Payment
	err := wait.NoError(func() error {
		stream, err := node.RouterClient.SendPaymentV2(ctx, req)
		if err != nil {
			return fmt.Errorf("unable to send payment: %v", err)
		}

		result, err = getPaymentResult(stream)
		if err != nil {
			return fmt.Errorf("unable to get payment result: %v",
				err)
		}

		if result.Status != lnrpc.Payment_SUCCEEDED {
			return fmt.Errorf("payment failed: %v", result.Status)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t.t, err)

	return result
}

// sendAndAssertFailure sends the given payment requests and asserts that the
// payment fails with the expected reason.
func sendAndAssertFailure(t *harnessTest, node *lntest.HarnessNode,
	req *routerrpc.SendPaymentRequest,
	failureReason lnrpc.PaymentFailureReason) *lnrpc.Payment {

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	stream, err := node.RouterClient.SendPaymentV2(ctx, req)
	require.NoError(t.t, err, "unable to send payment")

	result, err := getPaymentResult(stream)
	require.NoError(t.t, err, "unable to get payment result")

	require.Equal(
		t.t, lnrpc.Payment_FAILED, result.Status,
		"payment was expected to fail, but succeeded",
	)

	require.Equal(
		t.t, failureReason, result.FailureReason,
		"payment failureReason not matched",
	)

	return result
}

// getPaymentResult reads a final result from the stream and returns it.
func getPaymentResult(stream routerrpc.Router_SendPaymentV2Client) (
	*lnrpc.Payment, error) {

	for {
		payment, err := stream.Recv()
		if err != nil {
			return nil, err
		}

		if payment.Status != lnrpc.Payment_IN_FLIGHT {
			return payment, nil
		}
	}
}

// assertNumUTXOs waits for the given number of UTXOs to be available or fails
// if that isn't the case before the default timeout.
func assertNumUTXOs(t *testing.T, node *lntest.HarnessNode, expectedUtxos int) {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	err := wait.NoError(func() error {
		resp, err := node.ListUnspent( // nolint:staticcheck
			ctxt, &lnrpc.ListUnspentRequest{
				MinConfs: 1,
				MaxConfs: math.MaxInt32,
			},
		)
		if err != nil {
			return fmt.Errorf("error listing unspent: %v", err)
		}

		if len(resp.Utxos) != expectedUtxos {
			return fmt.Errorf("not enough UTXOs, got %d wanted %d",
				len(resp.Utxos), expectedUtxos)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t, err, "wait for listunspent")
}

// assertChannelPolicyUpdate checks that the required policy update has
// happened on the given node.
func assertChannelPolicyUpdate(t *testing.T, node *lntest.HarnessNode,
	advertisingNode string, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint, includeUnannounced bool) {

	require.NoError(
		t, node.WaitForChannelPolicyUpdate(
			advertisingNode, policy,
			chanPoint, includeUnannounced,
		), "error while waiting for channel update",
	)
}

func transactionInWallet(node *lntest.HarnessNode, txid chainhash.Hash) bool {
	txStr := txid.String()

	txResp, err := node.GetTransactions(
		context.Background(), &lnrpc.GetTransactionsRequest{},
	)
	if err != nil {
		return false
	}

	for _, txn := range txResp.Transactions {
		if txn.TxHash == txStr {
			return true
		}
	}

	return false
}

func assertTransactionInWallet(t *testing.T, node *lntest.HarnessNode, txID chainhash.Hash) {
	t.Helper()

	err := wait.Predicate(func() bool {
		return transactionInWallet(node, txID)
	}, defaultTimeout)
	require.NoError(
		t, err, fmt.Sprintf("transaction %v not found in wallet", txID),
	)
}

func assertTransactionNotInWallet(t *testing.T, node *lntest.HarnessNode,
	txID chainhash.Hash) {

	t.Helper()

	err := wait.Predicate(func() bool {
		return !transactionInWallet(node, txID)
	}, defaultTimeout)
	require.NoError(
		t, err, fmt.Sprintf("transaction %v found in wallet", txID),
	)
}

func assertAnchorOutputLost(t *harnessTest, node *lntest.HarnessNode,
	chanPoint wire.OutPoint) {

	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	err := wait.Predicate(func() bool {
		resp, pErr := node.PendingChannels(
			context.Background(), pendingChansRequest,
		)
		if pErr != nil {
			return false
		}

		for _, pendingChan := range resp.PendingForceClosingChannels {
			if pendingChan.Channel.ChannelPoint == chanPoint.String() {
				return (pendingChan.Anchor ==
					lnrpc.PendingChannelsResponse_ForceClosedChannel_LOST)
			}
		}

		return false
	}, defaultTimeout)
	require.NoError(t.t, err, "anchor doesn't show as being lost")
}

// assertNodeAnnouncement compares that two node announcements match.
func assertNodeAnnouncement(t *harnessTest, n1, n2 *lnrpc.NodeUpdate) {
	// Alias should match.
	require.Equal(t.t, n1.Alias, n2.Alias, "alias don't match")

	// Color should match.
	require.Equal(t.t, n1.Color, n2.Color, "color don't match")

	// NodeAddresses  should match.
	require.Equal(
		t.t, len(n1.NodeAddresses), len(n2.NodeAddresses),
		"node addresses don't match",
	)

	addrs := make(map[string]struct{}, len(n1.NodeAddresses))
	for _, nodeAddr := range n1.NodeAddresses {
		addrs[nodeAddr.Addr] = struct{}{}
	}

	for _, nodeAddr := range n2.NodeAddresses {
		if _, ok := addrs[nodeAddr.Addr]; !ok {
			t.Fatalf("address %v not found in node announcement",
				nodeAddr.Addr)
		}
	}
}

// assertUpdateNodeAnnouncementResponse is a helper function to assert
// the response expected values.
func assertUpdateNodeAnnouncementResponse(t *harnessTest,
	response *peersrpc.NodeAnnouncementUpdateResponse,
	expectedOps map[string]int) {

	require.Equal(
		t.t, len(response.Ops), len(expectedOps),
		"unexpected number of Ops updating dave's node announcement",
	)

	ops := make(map[string]int, len(response.Ops))
	for _, op := range response.Ops {
		ops[op.Entity] = len(op.Actions)
	}

	for k, v := range expectedOps {
		if v != ops[k] {
			t.Fatalf("unexpected number of actions for operation "+
				"%s: got %d wanted %d", k, ops[k], v)
		}
	}
}
