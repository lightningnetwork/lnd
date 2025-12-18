package lntest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest/miner"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// FindChannelOption is a functional type for an option that modifies a
// ListChannelsRequest.
type ListChannelOption func(r *lnrpc.ListChannelsRequest)

// WithPeerAliasLookup is an option for setting the peer alias lookup flag on a
// ListChannelsRequest.
func WithPeerAliasLookup() ListChannelOption {
	return func(r *lnrpc.ListChannelsRequest) {
		r.PeerAliasLookup = true
	}
}

// WaitForBlockchainSync waits until the node is synced to chain.
func (h *HarnessTest) WaitForBlockchainSync(hn *node.HarnessNode) {
	err := wait.NoError(func() error {
		resp := hn.RPC.GetInfo()
		if resp.SyncedToChain {
			return nil
		}

		return fmt.Errorf("%s is not synced to chain", hn.Name())
	}, DefaultTimeout)

	require.NoError(h, err, "timeout waiting for blockchain sync")
}

// WaitForBlockchainSyncTo waits until the node is synced to bestBlock.
func (h *HarnessTest) WaitForBlockchainSyncTo(hn *node.HarnessNode,
	bestBlock chainhash.Hash) {

	bestBlockHash := bestBlock.String()
	err := wait.NoError(func() error {
		resp := hn.RPC.GetInfo()
		if resp.SyncedToChain {
			if resp.BlockHash == bestBlockHash {
				return nil
			}

			return fmt.Errorf("%s's backend is synced to the "+
				"wrong block (expected=%s, actual=%s)",
				hn.Name(), bestBlockHash, resp.BlockHash)
		}

		return fmt.Errorf("%s is not synced to chain", hn.Name())
	}, DefaultTimeout)

	require.NoError(h, err, "timeout waiting for blockchain sync")
}

// AssertPeerConnected asserts that the given node b is connected to a.
func (h *HarnessTest) AssertPeerConnected(a, b *node.HarnessNode) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		resp := a.RPC.ListPeers()

		// If node B is seen in the ListPeers response from node A,
		// then we can return true as the connection has been fully
		// established.
		for _, peer := range resp.Peers {
			if peer.PubKey == b.PubKeyStr {
				return nil
			}
		}

		return fmt.Errorf("%s not found in %s's ListPeers",
			b.Name(), a.Name())
	}, DefaultTimeout)

	require.NoError(h, err, "unable to connect %s to %s, got error: "+
		"peers not connected within %v seconds",
		a.Name(), b.Name(), DefaultTimeout)
}

// ConnectNodes creates a connection between the two nodes and asserts the
// connection is succeeded.
func (h *HarnessTest) ConnectNodes(a, b *node.HarnessNode) {
	bobInfo := b.RPC.GetInfo()

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.Cfg.P2PAddr(),
		},
	}
	a.RPC.ConnectPeer(req)
	h.AssertConnected(a, b)
}

// ConnectNodesPerm creates a persistent connection between the two nodes and
// asserts the connection is succeeded.
func (h *HarnessTest) ConnectNodesPerm(a, b *node.HarnessNode) {
	bobInfo := b.RPC.GetInfo()

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.Cfg.P2PAddr(),
		},
		Perm: true,
	}
	a.RPC.ConnectPeer(req)
	h.AssertPeerConnected(a, b)
}

// DisconnectNodes disconnects the given two nodes and asserts the
// disconnection is succeeded. The request is made from node a and sent to node
// b.
func (h *HarnessTest) DisconnectNodes(a, b *node.HarnessNode) {
	bobInfo := b.RPC.GetInfo()
	a.RPC.DisconnectPeer(bobInfo.IdentityPubkey)

	// Assert disconnected.
	h.AssertPeerNotConnected(a, b)
}

// EnsureConnected will try to connect to two nodes, returning no error if they
// are already connected. If the nodes were not connected previously, this will
// behave the same as ConnectNodes. If a pending connection request has already
// been made, the method will block until the two nodes appear in each other's
// peers list, or until the DefaultTimeout expires.
func (h *HarnessTest) EnsureConnected(a, b *node.HarnessNode) {
	// errConnectionRequested is used to signal that a connection was
	// requested successfully, which is distinct from already being
	// connected to the peer.
	errConnectionRequested := "connection request in progress"

	// windowsErr is an error we've seen from windows build where
	// connecting to an already connected node gives such error from the
	// receiver side.
	windowsErr := "An established connection was aborted by the software " +
		"in your host machine."

	tryConnect := func(a, b *node.HarnessNode) error {
		bInfo := b.RPC.GetInfo()

		req := &lnrpc.ConnectPeerRequest{
			Addr: &lnrpc.LightningAddress{
				Pubkey: bInfo.IdentityPubkey,
				Host:   b.Cfg.P2PAddr(),
			},
		}

		ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
		defer cancel()

		_, err := a.RPC.LN.ConnectPeer(ctxt, req)

		// Request was successful.
		if err == nil {
			return nil
		}

		// If the two are already connected, we return early with no
		// error.
		if strings.Contains(err.Error(), "already connected to peer") {
			return nil
		}

		// Otherwise we log the error to console.
		h.Logf("EnsureConnected %s=>%s got err: %v", a.Name(),
			b.Name(), err)

		// If the connection is in process, we return no error.
		if strings.Contains(err.Error(), errConnectionRequested) {
			return nil
		}

		// We may get connection refused error if we happens to be in
		// the middle of a previous node disconnection, e.g., a restart
		// from one of the nodes.
		if strings.Contains(err.Error(), "connection refused") {
			return nil
		}

		// Check for windows error. If Alice connects to Bob, Alice
		// will throw "i/o timeout" and Bob will give windowsErr.
		if strings.Contains(err.Error(), windowsErr) {
			return nil
		}

		if strings.Contains(err.Error(), "i/o timeout") {
			return nil
		}

		return err
	}

	// Return any critical errors returned by either alice or bob.
	require.NoError(h, tryConnect(a, b), "connection failed between %s "+
		"and %s", a.Cfg.Name, b.Cfg.Name)

	// When Alice and Bob each makes a connection to the other side at the
	// same time, it's likely neither connections could succeed. Bob's
	// connection will be canceled by Alice since she has an outbound
	// connection to Bob already, and same happens to Alice's. Thus the two
	// connections cancel each other out.
	// TODO(yy): move this back when the above issue is fixed.
	// require.NoError(h, tryConnect(b, a), "connection failed between %s "+
	// 	"and %s", a.Cfg.Name, b.Cfg.Name)

	// Otherwise one or both requested a connection, so we wait for the
	// peers lists to reflect the connection.
	h.AssertPeerConnected(a, b)
	h.AssertPeerConnected(b, a)
}

// ConnectNodesNoAssert creates a connection from node A to node B.
func (h *HarnessTest) ConnectNodesNoAssert(a, b *node.HarnessNode) (
	*lnrpc.ConnectPeerResponse, error) {

	bobInfo := b.RPC.GetInfo()

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.Cfg.P2PAddr(),
		},
	}
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	return a.RPC.LN.ConnectPeer(ctxt, req)
}

// AssertNumEdges checks that an expected number of edges can be found in the
// node specified.
func (h *HarnessTest) AssertNumEdges(hn *node.HarnessNode,
	expected int, includeUnannounced bool) []*lnrpc.ChannelEdge {

	var edges []*lnrpc.ChannelEdge

	old := hn.State.Edge.Public
	if includeUnannounced {
		old = hn.State.Edge.Total
	}

	err := wait.NoError(func() error {
		req := &lnrpc.ChannelGraphRequest{
			IncludeUnannounced: includeUnannounced,
		}
		resp := hn.RPC.DescribeGraph(req)
		total := len(resp.Edges)

		if total-old == expected {
			if expected != 0 {
				// NOTE: assume edges come in ascending order
				// that the old edges are at the front of the
				// slice.
				edges = resp.Edges[old:]
			}

			return nil
		}

		return errNumNotMatched(hn.Name(), "num of channel edges",
			expected, total-old, total, old)
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while checking for edges")

	return edges
}

// ReceiveOpenChannelUpdate waits until a message is received on the stream or
// the timeout is reached.
func (h *HarnessTest) ReceiveOpenChannelUpdate(
	stream rpc.OpenChanClient) *lnrpc.OpenStatusUpdate {

	update, err := h.receiveOpenChannelUpdate(stream)
	require.NoError(h, err, "received err from open channel stream")

	return update
}

// ReceiveOpenChannelError waits for the expected error during the open channel
// flow from the peer or times out.
func (h *HarnessTest) ReceiveOpenChannelError(
	stream rpc.OpenChanClient, expectedErr error) {

	_, err := h.receiveOpenChannelUpdate(stream)
	require.Contains(h, err.Error(), expectedErr.Error(),
		"error not matched")
}

// receiveOpenChannelUpdate waits until a message or an error is received on
// the stream or the timeout is reached.
//
// TODO(yy): use generics to unify all receiving stream update once go@1.18 is
// used.
func (h *HarnessTest) receiveOpenChannelUpdate(
	stream rpc.OpenChanClient) (*lnrpc.OpenStatusUpdate, error) {

	chanMsg := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout waiting for open channel "+
			"update sent")
		return nil, nil

	case err := <-errChan:
		return nil, err

	case updateMsg := <-chanMsg:
		return updateMsg, nil
	}
}

// WaitForChannelOpenEvent waits for a notification that a channel is open by
// consuming a message from the passed open channel stream.
func (h HarnessTest) WaitForChannelOpenEvent(
	stream rpc.OpenChanClient) *lnrpc.ChannelPoint {

	// Consume one event.
	event := h.ReceiveOpenChannelUpdate(stream)

	resp, ok := event.Update.(*lnrpc.OpenStatusUpdate_ChanOpen)
	require.Truef(h, ok, "expected channel open update, instead got %v",
		resp)

	return resp.ChanOpen.ChannelPoint
}

// AssertChannelExists asserts that an active channel identified by the
// specified channel point exists from the point-of-view of the node.
func (h *HarnessTest) AssertChannelExists(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint) *lnrpc.Channel {

	return h.assertChannelStatus(hn, cp, true)
}

// AssertChannelActive checks if a channel identified by the specified channel
// point is active.
func (h *HarnessTest) AssertChannelActive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint) *lnrpc.Channel {

	return h.assertChannelStatus(hn, cp, true)
}

// AssertChannelInactive checks if a channel identified by the specified channel
// point is inactive.
func (h *HarnessTest) AssertChannelInactive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint) *lnrpc.Channel {

	return h.assertChannelStatus(hn, cp, false)
}

// assertChannelStatus asserts that a channel identified by the specified
// channel point exists from the point-of-view of the node and that it is either
// active or inactive depending on the value of the active parameter.
func (h *HarnessTest) assertChannelStatus(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, active bool) *lnrpc.Channel {

	var (
		channel *lnrpc.Channel
		err     error
	)

	err = wait.NoError(func() error {
		channel, err = h.findChannel(hn, cp)
		if err != nil {
			return err
		}

		// Check whether the channel is active, exit early if it is.
		if channel.Active == active {
			return nil
		}

		return fmt.Errorf("expected channel_active=%v, got %v",
			active, channel.Active)
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: timeout checking for channel point: %v",
		hn.Name(), h.OutPointFromChannelPoint(cp))

	return channel
}

// AssertOutputScriptClass checks that the specified transaction output has the
// expected script class.
func (h *HarnessTest) AssertOutputScriptClass(tx *btcutil.Tx,
	outputIndex uint32, scriptClass txscript.ScriptClass) {

	require.Greater(h, len(tx.MsgTx().TxOut), int(outputIndex))

	txOut := tx.MsgTx().TxOut[outputIndex]

	pkScript, err := txscript.ParsePkScript(txOut.PkScript)
	require.NoError(h, err)
	require.Equal(h, scriptClass, pkScript.Class())
}

// findChannel tries to find a target channel in the node using the given
// channel point.
func (h *HarnessTest) findChannel(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint,
	opts ...ListChannelOption) (*lnrpc.Channel, error) {

	// Get the funding point.
	fp := h.OutPointFromChannelPoint(chanPoint)

	req := &lnrpc.ListChannelsRequest{}

	for _, opt := range opts {
		opt(req)
	}

	channelInfo := hn.RPC.ListChannels(req)

	// Find the target channel.
	for _, channel := range channelInfo.Channels {
		if channel.ChannelPoint == fp.String() {
			return channel, nil
		}
	}

	return nil, fmt.Errorf("%s: channel not found using %s", hn.Name(),
		fp.String())
}

// ReceiveCloseChannelUpdate waits until a message or an error is received on
// the subscribe channel close stream or the timeout is reached.
func (h *HarnessTest) ReceiveCloseChannelUpdate(
	stream rpc.CloseChanClient) (*lnrpc.CloseStatusUpdate, error) {

	chanMsg := make(chan *lnrpc.CloseStatusUpdate)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout waiting for close channel "+
			"update sent")

		return nil, nil

	case err := <-errChan:
		return nil, fmt.Errorf("received err from close channel "+
			"stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg, nil
	}
}

type WaitingCloseChannel *lnrpc.PendingChannelsResponse_WaitingCloseChannel

// AssertChannelWaitingClose asserts that the given channel found in the node
// is waiting close. Returns the WaitingCloseChannel if found.
func (h *HarnessTest) AssertChannelWaitingClose(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) WaitingCloseChannel {

	var target WaitingCloseChannel

	op := h.OutPointFromChannelPoint(chanPoint)

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()

		for _, waitingClose := range resp.WaitingCloseChannels {
			if waitingClose.Channel.ChannelPoint == op.String() {
				target = waitingClose
				return nil
			}
		}

		return fmt.Errorf("%v: channel %s not found in waiting close",
			hn.Name(), op)
	}, DefaultTimeout)
	require.NoError(h, err, "assert channel waiting close timed out")

	return target
}

// AssertTopologyChannelClosed asserts a given channel is closed by checking
// the graph topology subscription of the specified node. Returns the closed
// channel update if found.
func (h *HarnessTest) AssertTopologyChannelClosed(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.ClosedChannelUpdate {

	closedChan, err := hn.Watcher.WaitForChannelClose(chanPoint)
	require.NoError(h, err, "failed to wait for channel close")

	return closedChan
}

// WaitForChannelCloseEvent waits for a notification that a channel is closed
// by consuming a message from the passed close channel stream. Returns the
// closing txid if found.
func (h HarnessTest) WaitForChannelCloseEvent(
	stream rpc.CloseChanClient) chainhash.Hash {

	// Consume one event.
	event, err := h.ReceiveCloseChannelUpdate(stream)
	require.NoError(h, err)

	resp, ok := event.Update.(*lnrpc.CloseStatusUpdate_ChanClose)
	require.Truef(h, ok, "expected channel close update, instead got %v",
		event.Update)

	txid, err := chainhash.NewHash(resp.ChanClose.ClosingTxid)
	require.NoErrorf(h, err, "wrong format found in closing txid: %v",
		resp.ChanClose.ClosingTxid)

	return *txid
}

// AssertNumWaitingClose checks that a PendingChannels response from the node
// reports the expected number of waiting close channels.
func (h *HarnessTest) AssertNumWaitingClose(hn *node.HarnessNode,
	num int) []*lnrpc.PendingChannelsResponse_WaitingCloseChannel {

	var channels []*lnrpc.PendingChannelsResponse_WaitingCloseChannel
	oldWaiting := hn.State.CloseChannel.WaitingClose

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()
		channels = resp.WaitingCloseChannels
		total := len(channels)

		got := total - oldWaiting
		if got == num {
			return nil
		}

		return errNumNotMatched(hn.Name(), "waiting close channels",
			num, got, total, oldWaiting)
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: assert waiting close timeout",
		hn.Name())

	return channels
}

// AssertNumPendingForceClose checks that a PendingChannels response from the
// node reports the expected number of pending force close channels.
func (h *HarnessTest) AssertNumPendingForceClose(hn *node.HarnessNode,
	num int) []*lnrpc.PendingChannelsResponse_ForceClosedChannel {

	var channels []*lnrpc.PendingChannelsResponse_ForceClosedChannel
	oldForce := hn.State.CloseChannel.PendingForceClose

	err := wait.NoError(func() error {
		// TODO(yy): we should be able to use `hn.RPC.PendingChannels`
		// here to avoid checking the RPC error. However, we may get a
		// `unable to find arbitrator` error from the rpc point, due to
		// a timing issue in rpcserver,
		// 1. `r.server.chanStateDB.FetchClosedChannels` fetches
		//    the pending force close channel.
		// 2. `r.arbitratorPopulateForceCloseResp` relies on the
		//    channel arbitrator to get the report, and,
		// 3. the arbitrator may be deleted due to the force close
		//    channel being resolved.
		// Somewhere along the line is missing a lock to keep the data
		// consistent.
		req := &lnrpc.PendingChannelsRequest{}
		resp, err := hn.RPC.LN.PendingChannels(h.runCtx, req)
		if err != nil {
			return fmt.Errorf("PendingChannels got: %w", err)
		}

		channels = resp.PendingForceClosingChannels
		total := len(channels)

		got := total - oldForce
		if got == num {
			return nil
		}

		return errNumNotMatched(hn.Name(), "pending force close "+
			"channels", num, got, total, oldForce)
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: assert pending force close timeout",
		hn.Name())

	return channels
}

// AssertStreamChannelCoopClosed reads an update from the close channel client
// stream and asserts that the mempool state and node's topology match a coop
// close. In specific,
// - assert the channel is waiting close and has the expected ChanStatusFlags.
// - assert the mempool has the closing txes and anchor sweeps.
// - mine a block and assert the closing txid is mined.
// - assert the node has zero waiting close channels.
// - assert the node has seen the channel close update.
func (h *HarnessTest) AssertStreamChannelCoopClosed(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, anchors bool,
	stream rpc.CloseChanClient) chainhash.Hash {

	// Assert the channel is waiting close.
	resp := h.AssertChannelWaitingClose(hn, cp)

	// Assert that the channel is in coop broadcasted.
	require.Contains(h, resp.Channel.ChanStatusFlags,
		channeldb.ChanStatusCoopBroadcasted.String(),
		"channel not coop broadcasted")

	// We'll now, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block. If there are anchors, we also expect an anchor sweep.
	expectedTxes := 1
	if anchors {
		expectedTxes = 2
	}
	block := h.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]

	// Consume one close event and assert the closing txid can be found in
	// the block.
	closingTxid := h.WaitForChannelCloseEvent(stream)
	h.AssertTxInBlock(block, closingTxid)

	// We should see zero waiting close channels now.
	h.AssertNumWaitingClose(hn, 0)

	// Finally, check that the node's topology graph has seen this channel
	// closed if it's a public channel.
	if !resp.Channel.Private {
		h.AssertTopologyChannelClosed(hn, cp)
	}

	return closingTxid
}

// AssertStreamChannelForceClosed reads an update from the close channel client
// stream and asserts that the mempool state and node's topology match a local
// force close. In specific,
//   - assert the channel is waiting close and has the expected ChanStatusFlags.
//   - assert the mempool has the closing txes.
//   - mine a block and assert the closing txid is mined.
//   - assert the channel is pending force close.
//   - assert the node has seen the channel close update.
//   - assert there's a pending anchor sweep request once the force close tx is
//     confirmed.
func (h *HarnessTest) AssertStreamChannelForceClosed(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, anchorSweep bool,
	stream rpc.CloseChanClient) chainhash.Hash {

	// Assert the channel is waiting close.
	resp := h.AssertChannelWaitingClose(hn, cp)

	// Assert that the channel is in local force broadcasted.
	require.Contains(h, resp.Channel.ChanStatusFlags,
		channeldb.ChanStatusLocalCloseInitiator.String(),
		"channel not coop broadcasted")

	// Get the closing txid.
	closeTxid, err := chainhash.NewHashFromStr(resp.ClosingTxid)
	require.NoError(h, err)

	// We'll now, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	closeTx := h.AssertTxInMempool(*closeTxid)
	h.MineBlockWithTx(closeTx)

	// Consume one close event and assert the closing txid can be found in
	// the block.
	closingTxid := h.WaitForChannelCloseEvent(stream)

	// We should see zero waiting close channels and 1 pending force close
	// channels now.
	h.AssertNumWaitingClose(hn, 0)
	h.AssertNumPendingForceClose(hn, 1)

	// Finally, check that the node's topology graph has seen this channel
	// closed if it's a public channel.
	if !resp.Channel.Private {
		h.AssertTopologyChannelClosed(hn, cp)
	}

	// Assert there's a pending anchor sweep.
	//
	// NOTE: We may have a local sweep here, that's why we use
	// AssertAtLeastNumPendingSweeps instead of AssertNumPendingSweeps.
	if anchorSweep {
		h.AssertAtLeastNumPendingSweeps(hn, 1)
	}

	return closingTxid
}

// AssertChannelPolicyUpdate checks that the required policy update has
// happened on the given node.
func (h *HarnessTest) AssertChannelPolicyUpdate(hn *node.HarnessNode,
	advertisingNode *node.HarnessNode, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint, includeUnannounced bool) {

	require.NoError(
		h, hn.Watcher.WaitForChannelPolicyUpdate(
			advertisingNode, policy,
			chanPoint, includeUnannounced,
		), "%s: error while waiting for channel update", hn.Name(),
	)
}

// WaitForGraphSync waits until the node is synced to graph or times out.
func (h *HarnessTest) WaitForGraphSync(hn *node.HarnessNode) {
	err := wait.NoError(func() error {
		resp := hn.RPC.GetInfo()
		if resp.SyncedToGraph {
			return nil
		}

		return fmt.Errorf("node not synced to graph")
	}, DefaultTimeout)
	require.NoError(h, err, "%s: timeout while sync to graph", hn.Name())
}

// AssertNumUTXOsWithConf waits for the given number of UTXOs with the
// specified confirmations range to be available or fails if that isn't the
// case before the default timeout.
func (h *HarnessTest) AssertNumUTXOsWithConf(hn *node.HarnessNode,
	expectedUtxos int, max, min int32) []*lnrpc.Utxo {

	var unconfirmed bool

	if max == 0 {
		unconfirmed = true
	}

	var utxos []*lnrpc.Utxo
	err := wait.NoError(func() error {
		req := &walletrpc.ListUnspentRequest{
			Account:         "",
			MaxConfs:        max,
			MinConfs:        min,
			UnconfirmedOnly: unconfirmed,
		}
		resp := hn.RPC.ListUnspent(req)
		total := len(resp.Utxos)

		if total == expectedUtxos {
			utxos = resp.Utxos

			return nil
		}

		desc := "has UTXOs:\n"
		for _, utxo := range resp.Utxos {
			desc += fmt.Sprintf("%v\n", utxo)
		}

		return fmt.Errorf("%s: assert num of UTXOs failed: want %d, "+
			"got: %d, %s", hn.Name(), expectedUtxos, total, desc)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout waiting for UTXOs")

	return utxos
}

// AssertNumUTXOsUnconfirmed asserts the expected num of unconfirmed utxos are
// seen.
func (h *HarnessTest) AssertNumUTXOsUnconfirmed(hn *node.HarnessNode,
	num int) []*lnrpc.Utxo {

	return h.AssertNumUTXOsWithConf(hn, num, 0, 0)
}

// AssertNumUTXOsConfirmed asserts the expected num of confirmed utxos are
// seen, which means the returned utxos have at least one confirmation.
func (h *HarnessTest) AssertNumUTXOsConfirmed(hn *node.HarnessNode,
	num int) []*lnrpc.Utxo {

	return h.AssertNumUTXOsWithConf(hn, num, math.MaxInt32, 1)
}

// AssertNumUTXOs asserts the expected num of utxos are seen, including
// confirmed and unconfirmed outputs.
func (h *HarnessTest) AssertNumUTXOs(hn *node.HarnessNode,
	num int) []*lnrpc.Utxo {

	return h.AssertNumUTXOsWithConf(hn, num, math.MaxInt32, 0)
}

// getUTXOs gets the number of newly created UTOXs within the current test
// scope.
func (h *HarnessTest) getUTXOs(hn *node.HarnessNode, account string,
	max, min int32) []*lnrpc.Utxo {

	var unconfirmed bool

	if max == 0 {
		unconfirmed = true
	}

	req := &walletrpc.ListUnspentRequest{
		Account:         account,
		MaxConfs:        max,
		MinConfs:        min,
		UnconfirmedOnly: unconfirmed,
	}
	resp := hn.RPC.ListUnspent(req)

	return resp.Utxos
}

// GetUTXOs returns all the UTXOs for the given node's account, including
// confirmed and unconfirmed.
func (h *HarnessTest) GetUTXOs(hn *node.HarnessNode,
	account string) []*lnrpc.Utxo {

	return h.getUTXOs(hn, account, math.MaxInt32, 0)
}

// GetUTXOsConfirmed returns the confirmed UTXOs for the given node's account.
func (h *HarnessTest) GetUTXOsConfirmed(hn *node.HarnessNode,
	account string) []*lnrpc.Utxo {

	return h.getUTXOs(hn, account, math.MaxInt32, 1)
}

// GetUTXOsUnconfirmed returns the unconfirmed UTXOs for the given node's
// account.
func (h *HarnessTest) GetUTXOsUnconfirmed(hn *node.HarnessNode,
	account string) []*lnrpc.Utxo {

	return h.getUTXOs(hn, account, 0, 0)
}

// WaitForBalanceConfirmed waits until the node sees the expected confirmed
// balance in its wallet.
func (h *HarnessTest) WaitForBalanceConfirmed(hn *node.HarnessNode,
	expected btcutil.Amount) {

	var lastBalance btcutil.Amount
	err := wait.NoError(func() error {
		resp := hn.RPC.WalletBalance()

		lastBalance = btcutil.Amount(resp.ConfirmedBalance)
		if lastBalance == expected {
			return nil
		}

		return fmt.Errorf("expected %v, only have %v", expected,
			lastBalance)
	}, DefaultTimeout)

	require.NoError(h, err, "timeout waiting for confirmed balances")
}

// WaitForBalanceUnconfirmed waits until the node sees the expected unconfirmed
// balance in its wallet.
func (h *HarnessTest) WaitForBalanceUnconfirmed(hn *node.HarnessNode,
	expected btcutil.Amount) {

	var lastBalance btcutil.Amount
	err := wait.NoError(func() error {
		resp := hn.RPC.WalletBalance()

		lastBalance = btcutil.Amount(resp.UnconfirmedBalance)
		if lastBalance == expected {
			return nil
		}

		return fmt.Errorf("expected %v, only have %v", expected,
			lastBalance)
	}, DefaultTimeout)

	require.NoError(h, err, "timeout waiting for unconfirmed balances")
}

// Random32Bytes generates a random 32 bytes which can be used as a pay hash,
// preimage, etc.
func (h *HarnessTest) Random32Bytes() []byte {
	randBuf := make([]byte, lntypes.HashSize)

	_, err := rand.Read(randBuf)
	require.NoErrorf(h, err, "internal error, cannot generate random bytes")

	return randBuf
}

// RandomPreimage generates a random preimage which can be used as a payment
// preimage.
func (h *HarnessTest) RandomPreimage() lntypes.Preimage {
	var preimage lntypes.Preimage
	copy(preimage[:], h.Random32Bytes())

	return preimage
}

// DecodeAddress decodes a given address and asserts there's no error.
func (h *HarnessTest) DecodeAddress(addr string) btcutil.Address {
	resp, err := btcutil.DecodeAddress(addr, miner.HarnessNetParams)
	require.NoError(h, err, "DecodeAddress failed")

	return resp
}

// PayToAddrScript creates a new script from the given address and asserts
// there's no error.
func (h *HarnessTest) PayToAddrScript(addr btcutil.Address) []byte {
	addrScript, err := txscript.PayToAddrScript(addr)
	require.NoError(h, err, "PayToAddrScript failed")

	return addrScript
}

// AssertChannelBalanceResp makes a ChannelBalance request and checks the
// returned response matches the expected.
func (h *HarnessTest) AssertChannelBalanceResp(hn *node.HarnessNode,
	expected *lnrpc.ChannelBalanceResponse) {

	resp := hn.RPC.ChannelBalance()

	// Ignore custom channel data of both expected and actual responses.
	expected.CustomChannelData = nil
	resp.CustomChannelData = nil

	require.True(h, proto.Equal(expected, resp), "balance is incorrect "+
		"got: %v, want: %v", resp, expected)
}

// GetChannelByChanPoint tries to find a channel matching the channel point and
// asserts. It returns the channel found.
func (h *HarnessTest) GetChannelByChanPoint(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.Channel {

	channel, err := h.findChannel(hn, chanPoint)
	require.NoErrorf(h, err, "channel not found using %v", chanPoint)

	return channel
}

// GetChannelCommitType retrieves the active channel commitment type for the
// given chan point.
func (h *HarnessTest) GetChannelCommitType(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) lnrpc.CommitmentType {

	c := h.GetChannelByChanPoint(hn, chanPoint)

	return c.CommitmentType
}

// AssertNumPendingOpenChannels asserts that a given node have the expected
// number of pending open channels.
func (h *HarnessTest) AssertNumPendingOpenChannels(hn *node.HarnessNode,
	expected int) []*lnrpc.PendingChannelsResponse_PendingOpenChannel {

	var channels []*lnrpc.PendingChannelsResponse_PendingOpenChannel

	oldNum := hn.State.OpenChannel.Pending

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()
		channels = resp.PendingOpenChannels
		total := len(channels)

		numChans := total - oldNum

		if numChans != expected {
			return errNumNotMatched(hn.Name(),
				"pending open channels", expected,
				numChans, total, oldNum)
		}

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "num of pending open channels not match")

	return channels
}

// AssertNodesNumPendingOpenChannels asserts that both of the nodes have the
// expected number of pending open channels.
func (h *HarnessTest) AssertNodesNumPendingOpenChannels(a, b *node.HarnessNode,
	expected int) {

	h.AssertNumPendingOpenChannels(a, expected)
	h.AssertNumPendingOpenChannels(b, expected)
}

// AssertPaymentStatusFromStream takes a client stream and asserts the payment
// is in desired status before default timeout. The payment found is returned
// once succeeded.
func (h *HarnessTest) AssertPaymentStatusFromStream(stream rpc.PaymentClient,
	status lnrpc.Payment_PaymentStatus) *lnrpc.Payment {

	return h.assertPaymentStatusWithTimeout(
		stream, status, wait.PaymentTimeout,
	)
}

// AssertPaymentSucceedWithTimeout asserts that a payment is succeeded within
// the specified timeout.
func (h *HarnessTest) AssertPaymentSucceedWithTimeout(stream rpc.PaymentClient,
	timeout time.Duration) *lnrpc.Payment {

	return h.assertPaymentStatusWithTimeout(
		stream, lnrpc.Payment_SUCCEEDED, timeout,
	)
}

// assertPaymentStatusWithTimeout takes a client stream and asserts the payment
// is in desired status before the specified timeout. The payment found is
// returned once succeeded.
func (h *HarnessTest) assertPaymentStatusWithTimeout(stream rpc.PaymentClient,
	status lnrpc.Payment_PaymentStatus,
	timeout time.Duration) *lnrpc.Payment {

	var target *lnrpc.Payment
	err := wait.NoError(func() error {
		// Consume one message. This will raise an error if the message
		// is not received within DefaultTimeout.
		payment, err := h.receivePaymentUpdateWithTimeout(
			stream, timeout,
		)
		if err != nil {
			return fmt.Errorf("received error from payment "+
				"stream: %s", err)
		}

		// Return if the desired payment state is reached.
		if payment.Status == status {
			target = payment

			return nil
		}

		// Return the err so that it can be used for debugging when
		// timeout is reached.
		return fmt.Errorf("payment %v status, got %v, want %v",
			payment.PaymentHash, payment.Status, status)
	}, timeout)

	require.NoError(h, err, "timeout while waiting payment")

	return target
}

// ReceivePaymentUpdate waits until a message is received on the payment client
// stream or the timeout is reached.
func (h *HarnessTest) ReceivePaymentUpdate(
	stream rpc.PaymentClient) (*lnrpc.Payment, error) {

	return h.receivePaymentUpdateWithTimeout(stream, DefaultTimeout)
}

// receivePaymentUpdateWithTimeout waits until a message is received on the
// payment client stream or the timeout is reached.
func (h *HarnessTest) receivePaymentUpdateWithTimeout(stream rpc.PaymentClient,
	timeout time.Duration) (*lnrpc.Payment, error) {

	chanMsg := make(chan *lnrpc.Payment, 1)
	errChan := make(chan error, 1)

	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err

			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(timeout):
		require.Fail(h, "timeout", "timeout waiting for payment update")
		return nil, nil

	case err := <-errChan:
		return nil, err

	case updateMsg := <-chanMsg:
		return updateMsg, nil
	}
}

// AssertInvoiceSettled asserts a given invoice specified by its payment
// address is settled.
func (h *HarnessTest) AssertInvoiceSettled(hn *node.HarnessNode, addr []byte) {
	msg := &invoicesrpc.LookupInvoiceMsg{
		InvoiceRef: &invoicesrpc.LookupInvoiceMsg_PaymentAddr{
			PaymentAddr: addr,
		},
	}

	err := wait.NoError(func() error {
		invoice := hn.RPC.LookupInvoiceV2(msg)
		if invoice.State == lnrpc.Invoice_SETTLED {
			return nil
		}

		return fmt.Errorf("%s: invoice with payment address %x not "+
			"settled", hn.Name(), addr)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout waiting for invoice settled state")
}

// AssertNodeNumChannels polls the provided node's list channels rpc until it
// reaches the desired number of total channels.
func (h *HarnessTest) AssertNodeNumChannels(hn *node.HarnessNode,
	numChannels int) {

	// Get the total number of channels.
	old := hn.State.OpenChannel.Active + hn.State.OpenChannel.Inactive

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		chanInfo := hn.RPC.ListChannels(&lnrpc.ListChannelsRequest{})

		// Return true if the query returned the expected number of
		// channels.
		num := len(chanInfo.Channels) - old
		if num != numChannels {
			return fmt.Errorf("expected %v channels, got %v",
				numChannels, num)
		}

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "timeout checking node's num of channels")
}

// AssertChannelLocalBalance checks the local balance of the given channel is
// expected. The channel found using the specified channel point is returned.
func (h *HarnessTest) AssertChannelLocalBalance(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, balance int64) *lnrpc.Channel {

	var result *lnrpc.Channel

	// Get the funding point.
	err := wait.NoError(func() error {
		// Find the target channel first.
		target, err := h.findChannel(hn, cp)

		// Exit early if the channel is not found.
		if err != nil {
			return fmt.Errorf("check balance failed: %w", err)
		}

		result = target

		// Check local balance.
		if target.LocalBalance == balance {
			return nil
		}

		return fmt.Errorf("balance is incorrect, got %v, expected %v",
			target.LocalBalance, balance)
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while checking for balance")

	return result
}

// AssertChannelNumUpdates checks the num of updates is expected from the given
// channel.
func (h *HarnessTest) AssertChannelNumUpdates(hn *node.HarnessNode,
	num uint64, cp *lnrpc.ChannelPoint) {

	old := int(hn.State.OpenChannel.NumUpdates)

	// Find the target channel first.
	target, err := h.findChannel(hn, cp)
	require.NoError(h, err, "unable to find channel")

	err = wait.NoError(func() error {
		total := int(target.NumUpdates)
		if total-old == int(num) {
			return nil
		}

		return errNumNotMatched(hn.Name(), "channel updates",
			int(num), total-old, total, old)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout while checking for num of updates")
}

// AssertNumActiveHtlcs asserts that a given number of HTLCs are seen in the
// node's channels.
func (h *HarnessTest) AssertNumActiveHtlcs(hn *node.HarnessNode, num int) {
	old := hn.State.HTLC

	err := wait.NoError(func() error {
		// pendingHTLCs is used to print unacked HTLCs, if found.
		var pendingHTLCs []string

		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		req := &lnrpc.ListChannelsRequest{}
		nodeChans := hn.RPC.ListChannels(req)

		total := 0
		for _, channel := range nodeChans.Channels {
			for _, htlc := range channel.PendingHtlcs {
				if htlc.LockedIn {
					total++
				}

				rHash := fmt.Sprintf("%x", htlc.HashLock)
				pendingHTLCs = append(pendingHTLCs, rHash)
			}
		}
		if total-old != num {
			desc := fmt.Sprintf("active HTLCs: unacked HTLCs: %v",
				pendingHTLCs)

			return errNumNotMatched(hn.Name(), desc,
				num, total-old, total, old)
		}

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s timeout checking num active htlcs",
		hn.Name())
}

// AssertIncomingHTLCActive asserts the node has a pending incoming HTLC in the
// given channel. Returns the HTLC if found and active.
func (h *HarnessTest) AssertIncomingHTLCActive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, payHash []byte) *lnrpc.HTLC {

	return h.assertHTLCActive(hn, cp, payHash, true)
}

// AssertOutgoingHTLCActive asserts the node has a pending outgoing HTLC in the
// given channel. Returns the HTLC if found and active.
func (h *HarnessTest) AssertOutgoingHTLCActive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, payHash []byte) *lnrpc.HTLC {

	return h.assertHTLCActive(hn, cp, payHash, false)
}

// assertHLTCActive asserts the node has a pending HTLC in the given channel.
// Returns the HTLC if found and active.
func (h *HarnessTest) assertHTLCActive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, payHash []byte, incoming bool) *lnrpc.HTLC {

	var result *lnrpc.HTLC
	target := hex.EncodeToString(payHash)

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		ch := h.GetChannelByChanPoint(hn, cp)

		// Check all payment hashes active for this channel.
		for _, htlc := range ch.PendingHtlcs {
			rHash := hex.EncodeToString(htlc.HashLock)
			if rHash != target {
				continue
			}

			// If the payment hash is found, check the incoming
			// field.
			if htlc.Incoming == incoming {
				// Return the result if it's locked in.
				if htlc.LockedIn {
					result = htlc
					return nil
				}

				return fmt.Errorf("htlc(%x) not locked in",
					payHash)
			}

			// Otherwise we do have the HTLC but its direction is
			// not right.
			have, want := "outgoing", "incoming"
			if htlc.Incoming {
				have, want = "incoming", "outgoing"
			}

			return fmt.Errorf("htlc(%x) has wrong direction - "+
				"want: %s, have: %s", payHash, want, have)
		}

		return fmt.Errorf("htlc not found using payHash %x", payHash)
	}, DefaultTimeout)
	require.NoError(h, err, "%s: timeout checking pending HTLC", hn.Name())

	return result
}

// AssertHLTCNotActive asserts the node doesn't have a pending HTLC in the
// given channel, which mean either the HTLC never exists, or it was pending
// and now settled. Returns the HTLC if found and active.
//
// NOTE: to check a pending HTLC becoming settled, first use AssertHLTCActive
// then follow this check.
func (h *HarnessTest) AssertHTLCNotActive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, payHash []byte) *lnrpc.HTLC {

	var result *lnrpc.HTLC
	target := hex.EncodeToString(payHash)

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		ch := h.GetChannelByChanPoint(hn, cp)

		// Check all payment hashes active for this channel.
		for _, htlc := range ch.PendingHtlcs {
			h := hex.EncodeToString(htlc.HashLock)

			// Break if found the htlc.
			if h == target {
				result = htlc
				break
			}
		}

		// If we've found nothing, we're done.
		if result == nil {
			return nil
		}

		// Otherwise return an error.
		return fmt.Errorf("node [%s:%x] still has: the payHash %x",
			hn.Name(), hn.PubKey[:], payHash)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking pending HTLC")

	return result
}

// ReceiveSingleInvoice waits until a message is received on the subscribe
// single invoice stream or the timeout is reached.
func (h *HarnessTest) ReceiveSingleInvoice(
	stream rpc.SingleInvoiceClient) *lnrpc.Invoice {

	chanMsg := make(chan *lnrpc.Invoice, 1)
	errChan := make(chan error, 1)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err

			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout receiving single invoice")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// AssertInvoiceState takes a single invoice subscription stream and asserts
// that a given invoice has became the desired state before timeout and returns
// the invoice found.
func (h *HarnessTest) AssertInvoiceState(stream rpc.SingleInvoiceClient,
	state lnrpc.Invoice_InvoiceState) *lnrpc.Invoice {

	var invoice *lnrpc.Invoice

	err := wait.NoError(func() error {
		invoice = h.ReceiveSingleInvoice(stream)
		if invoice.State == state {
			return nil
		}

		return fmt.Errorf("mismatched invoice state, want %v, got %v",
			state, invoice.State)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout waiting for invoice state: %v", state)

	return invoice
}

// assertAllTxesSpendFrom asserts that all txes in the list spend from the
// given tx.
func (h *HarnessTest) AssertAllTxesSpendFrom(txes []*wire.MsgTx,
	prevTxid chainhash.Hash) {

	for _, tx := range txes {
		if tx.TxIn[0].PreviousOutPoint.Hash != prevTxid {
			require.Failf(h, "", "tx %v did not spend from %v",
				tx.TxHash(), prevTxid)
		}
	}
}

// AssertTxSpendFrom asserts that a given tx is spent from a previous tx.
func (h *HarnessTest) AssertTxSpendFrom(tx *wire.MsgTx,
	prevTxid chainhash.Hash) {

	if tx.TxIn[0].PreviousOutPoint.Hash != prevTxid {
		require.Failf(h, "", "tx %v did not spend from %v",
			tx.TxHash(), prevTxid)
	}
}

type PendingForceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel

// AssertChannelPendingForceClose asserts that the given channel found in the
// node is pending force close. Returns the PendingForceClose if found.
func (h *HarnessTest) AssertChannelPendingForceClose(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) PendingForceClose {

	var target PendingForceClose

	op := h.OutPointFromChannelPoint(chanPoint)

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()

		forceCloseChans := resp.PendingForceClosingChannels
		for _, ch := range forceCloseChans {
			if ch.Channel.ChannelPoint == op.String() {
				target = ch

				return nil
			}
		}

		return fmt.Errorf("%v: channel %s not found in pending "+
			"force close", hn.Name(), chanPoint)
	}, DefaultTimeout)
	require.NoError(h, err, "assert pending force close timed out")

	return target
}

// AssertNumHTLCsAndStage takes a pending force close channel's channel point
// and asserts the expected number of pending HTLCs and HTLC stage are matched.
func (h *HarnessTest) AssertNumHTLCsAndStage(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, num int, stage uint32) {

	// Get the channel output point.
	cp := h.OutPointFromChannelPoint(chanPoint)

	var target PendingForceClose
	checkStage := func() error {
		resp := hn.RPC.PendingChannels()
		if len(resp.PendingForceClosingChannels) == 0 {
			return fmt.Errorf("zero pending force closing channels")
		}

		for _, ch := range resp.PendingForceClosingChannels {
			if ch.Channel.ChannelPoint == cp.String() {
				target = ch

				break
			}
		}

		if target == nil {
			return fmt.Errorf("cannot find pending force closing "+
				"channel using %v", cp)
		}

		if target.LimboBalance == 0 {
			return fmt.Errorf("zero limbo balance")
		}

		if len(target.PendingHtlcs) != num {
			return fmt.Errorf("got %d pending htlcs, want %d, %s",
				len(target.PendingHtlcs), num,
				lnutils.SpewLogClosure(target.PendingHtlcs)())
		}

		for _, htlc := range target.PendingHtlcs {
			if htlc.Stage == stage {
				continue
			}

			return fmt.Errorf("HTLC %s got stage: %v, "+
				"want stage: %v", htlc.Outpoint, htlc.Stage,
				stage)
		}

		return nil
	}

	require.NoErrorf(h, wait.NoError(checkStage, DefaultTimeout),
		"timeout waiting for htlc stage")
}

// findPayment queries the payment from the node's ListPayments which matches
// the specified preimage hash.
func (h *HarnessTest) findPayment(hn *node.HarnessNode,
	paymentHash string) (*lnrpc.Payment, error) {

	req := &lnrpc.ListPaymentsRequest{IncludeIncomplete: true}
	paymentsResp := hn.RPC.ListPayments(req)

	for _, p := range paymentsResp.Payments {
		if p.PaymentHash == paymentHash {
			return p, nil
		}
	}

	return nil, fmt.Errorf("payment %v cannot be found", paymentHash)
}

// PaymentCheck is a function that checks a payment for a specific condition.
type PaymentCheck func(*lnrpc.Payment) error

// AssertPaymentStatus asserts that the given node list a payment with the given
// payment hash has the expected status. It also checks that the payment has the
// expected preimage, which is empty when it's not settled and matches the given
// preimage when it's succeeded.
func (h *HarnessTest) AssertPaymentStatus(hn *node.HarnessNode,
	payHash lntypes.Hash, status lnrpc.Payment_PaymentStatus,
	checks ...PaymentCheck) *lnrpc.Payment {

	var target *lnrpc.Payment

	err := wait.NoError(func() error {
		p, err := h.findPayment(hn, payHash.String())
		if err != nil {
			return err
		}

		if status == p.Status {
			target = p
			return nil
		}

		return fmt.Errorf("payment: %v status not match, want %s "+
			"got %s", payHash, status, p.Status)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking payment status")

	switch status {
	// If this expected status is SUCCEEDED, we expect the final
	// preimage.
	case lnrpc.Payment_SUCCEEDED:
		preimage, err := lntypes.MakePreimageFromStr(
			target.PaymentPreimage,
		)
		require.NoError(h, err, "fail to make preimage")
		require.Equal(h, payHash, preimage.Hash(), "preimage not match")

	// Otherwise we expect an all-zero preimage.
	default:
		require.Equal(h, (lntypes.Preimage{}).String(),
			target.PaymentPreimage, "expected zero preimage")
	}

	// Perform any additional checks on the payment.
	for _, check := range checks {
		require.NoError(h, check(target))
	}

	return target
}

// AssertPaymentFailureReason asserts that the given node lists a payment with
// the given preimage which has the expected failure reason.
func (h *HarnessTest) AssertPaymentFailureReason(
	hn *node.HarnessNode, preimage lntypes.Preimage,
	reason lnrpc.PaymentFailureReason) *lnrpc.Payment {

	var payment *lnrpc.Payment

	payHash := preimage.Hash()
	err := wait.NoError(func() error {
		p, err := h.findPayment(hn, payHash.String())
		if err != nil {
			return err
		}

		payment = p

		if reason == p.FailureReason {
			return nil
		}

		return fmt.Errorf("payment: %v failure reason not match, "+
			"want %s(%d) got %s(%d)", payHash, reason, reason,
			p.FailureReason, p.FailureReason)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking payment failure reason")

	return payment
}

// AssertPaymentFailureReasonAny asserts that the given node lists a payment
// with the given preimage which has one of the expected failure reasons.
func (h *HarnessTest) AssertPaymentFailureReasonAny(
	hn *node.HarnessNode, preimage lntypes.Preimage,
	reasons ...lnrpc.PaymentFailureReason) *lnrpc.Payment {

	var payment *lnrpc.Payment

	payHash := preimage.Hash()
	err := wait.NoError(func() error {
		p, err := h.findPayment(hn, payHash.String())
		if err != nil {
			return err
		}

		payment = p

		// Check if the payment failure reason matches any of the
		// expected reasons.
		for _, reason := range reasons {
			if reason == p.FailureReason {
				return nil
			}
		}

		return fmt.Errorf("payment: %v failure reason not match, "+
			"want one of %v, got %s(%d)", payHash, reasons,
			p.FailureReason, p.FailureReason)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking payment failure reason")

	return payment
}

// AssertActiveNodesSynced asserts all active nodes have synced to the chain.
func (h *HarnessTest) AssertActiveNodesSynced() {
	for _, node := range h.manager.activeNodes {
		h.WaitForBlockchainSync(node)
	}
}

// AssertActiveNodesSyncedTo asserts all active nodes have synced to the
// provided bestBlock.
func (h *HarnessTest) AssertActiveNodesSyncedTo(bestBlock chainhash.Hash) {
	for _, node := range h.manager.activeNodes {
		h.WaitForBlockchainSyncTo(node, bestBlock)
	}
}

// AssertPeerNotConnected asserts that the given node b is not connected to a.
func (h *HarnessTest) AssertPeerNotConnected(a, b *node.HarnessNode) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		resp := a.RPC.ListPeers()

		// If node B is seen in the ListPeers response from node A,
		// then we return false as the connection has been fully
		// established.
		for _, peer := range resp.Peers {
			if peer.PubKey == b.PubKeyStr {
				return fmt.Errorf("peers %s and %s still "+
					"connected", a.Name(), b.Name())
			}
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking peers not connected")
}

// AssertNotConnected asserts that two peers are not connected.
func (h *HarnessTest) AssertNotConnected(a, b *node.HarnessNode) {
	// Sleep one second before the assertion to make sure that when there's
	// a RPC call to connect, that RPC call is finished before the
	// assertion.
	time.Sleep(1 * time.Second)

	h.AssertPeerNotConnected(a, b)
	h.AssertPeerNotConnected(b, a)
}

// AssertConnected asserts that two peers are connected.
func (h *HarnessTest) AssertConnected(a, b *node.HarnessNode) {
	h.AssertPeerConnected(a, b)
	h.AssertPeerConnected(b, a)
}

// AssertAmountPaid checks that the ListChannels command of the provided
// node list the total amount sent and received as expected for the
// provided channel.
func (h *HarnessTest) AssertAmountPaid(channelName string, hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, amountSent, amountReceived int64) {

	checkAmountPaid := func() error {
		// Find the targeted channel.
		channel, err := h.findChannel(hn, chanPoint)
		if err != nil {
			return fmt.Errorf("assert amount failed: %w", err)
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

	// As far as HTLC inclusion in commitment transaction might be
	// postponed we will try to check the balance couple of times,
	// and then if after some period of time we receive wrong
	// balance return the error.
	err := wait.NoError(checkAmountPaid, DefaultTimeout)
	require.NoError(h, err, "timeout while checking amount paid")
}

// AssertLastHTLCError checks that the last sent HTLC of the last payment sent
// by the given node failed with the expected failure code.
func (h *HarnessTest) AssertLastHTLCError(hn *node.HarnessNode,
	code lnrpc.Failure_FailureCode) {

	// Use -1 to specify the last HTLC.
	h.assertHTLCError(hn, code, -1)
}

// AssertFirstHTLCError checks that the first HTLC of the last payment sent
// by the given node failed with the expected failure code.
func (h *HarnessTest) AssertFirstHTLCError(hn *node.HarnessNode,
	code lnrpc.Failure_FailureCode) {

	// Use 0 to specify the first HTLC.
	h.assertHTLCError(hn, code, 0)
}

// assertLastHTLCError checks that the HTLC at the specified index of the last
// payment sent by the given node failed with the expected failure code.
func (h *HarnessTest) assertHTLCError(hn *node.HarnessNode,
	code lnrpc.Failure_FailureCode, index int) {

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
	}

	err := wait.NoError(func() error {
		paymentsResp := hn.RPC.ListPayments(req)

		payments := paymentsResp.Payments
		if len(payments) == 0 {
			return fmt.Errorf("no payments found")
		}

		payment := payments[len(payments)-1]
		htlcs := payment.Htlcs
		if len(htlcs) == 0 {
			return fmt.Errorf("no htlcs found")
		}

		// If the index is greater than 0, check we have enough htlcs.
		if index > 0 && len(htlcs) <= index {
			return fmt.Errorf("not enough htlcs")
		}

		// If index is less than or equal to 0, we will read the last
		// htlc.
		if index <= 0 {
			index = len(htlcs) - 1
		}

		htlc := htlcs[index]

		// The htlc must have a status of failed.
		if htlc.Status != lnrpc.HTLCAttempt_FAILED {
			return fmt.Errorf("htlc should be failed")
		}
		// The failure field must not be empty.
		if htlc.Failure == nil {
			return fmt.Errorf("expected htlc failure")
		}

		// Exit if the expected code is found.
		if htlc.Failure.Code == code {
			return nil
		}

		return fmt.Errorf("unexpected failure code")
	}, DefaultTimeout)

	require.NoError(h, err, "timeout checking HTLC error")
}

// AssertZombieChannel asserts that a given channel found using the chanID is
// marked as zombie.
func (h *HarnessTest) AssertZombieChannel(hn *node.HarnessNode, chanID uint64) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	err := wait.NoError(func() error {
		_, err := hn.RPC.LN.GetChanInfo(
			ctxt, &lnrpc.ChanInfoRequest{ChanId: chanID},
		)
		if err == nil {
			return fmt.Errorf("expected error but got nil")
		}

		if !strings.Contains(err.Error(), "marked as zombie") {
			return fmt.Errorf("expected error to contain '%s' but "+
				"was '%v'", "marked as zombie", err)
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err, "timeout while checking zombie channel")
}

// AssertNotInGraph asserts that a given channel is either not found at all in
// the graph or that it has been marked as a zombie.
func (h *HarnessTest) AssertNotInGraph(hn *node.HarnessNode, chanID uint64) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	err := wait.NoError(func() error {
		_, err := hn.RPC.LN.GetChanInfo(
			ctxt, &lnrpc.ChanInfoRequest{ChanId: chanID},
		)
		if err == nil {
			return fmt.Errorf("expected error but got nil")
		}

		switch {
		case strings.Contains(err.Error(), "marked as zombie"):
			return nil

		case strings.Contains(err.Error(), "edge not found"):
			return nil

		default:
			return fmt.Errorf("expected error to contain either "+
				"'%s' or '%s' but was: '%v'", "marked as i"+
				"zombie", "edge not found", err)
		}
	}, DefaultTimeout)
	require.NoError(h, err, "timeout while checking that channel is not "+
		"found in graph")
}

// AssertChannelInGraphDB asserts that a given channel is found in the graph db.
func (h *HarnessTest) AssertChannelInGraphDB(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.ChannelEdge {

	ctxt, cancel := context.WithCancel(h.runCtx)
	defer cancel()

	var edge *lnrpc.ChannelEdge

	op := h.OutPointFromChannelPoint(chanPoint)
	err := wait.NoError(func() error {
		resp, err := hn.RPC.LN.GetChanInfo(
			ctxt, &lnrpc.ChanInfoRequest{
				ChanPoint: op.String(),
			},
		)
		if err != nil {
			return fmt.Errorf("channel %s not found in graph: %w",
				op, err)
		}

		// Make sure the policies are populated, otherwise this edge
		// cannot be used for routing.
		if resp.Node1Policy == nil {
			return fmt.Errorf("channel %s has no policy1", op)
		}

		if resp.Node2Policy == nil {
			return fmt.Errorf("channel %s has no policy2", op)
		}

		edge = resp

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "%s: timeout finding channel in graph",
		hn.Name())

	return edge
}

// AssertChannelInGraphCache asserts a given channel is found in the graph
// cache.
func (h *HarnessTest) AssertChannelInGraphCache(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.ChannelEdge {

	var edge *lnrpc.ChannelEdge

	req := &lnrpc.ChannelGraphRequest{IncludeUnannounced: true}
	cpStr := channelPointStr(chanPoint)

	err := wait.NoError(func() error {
		chanGraph := hn.RPC.DescribeGraph(req)

		// Iterate all the known edges, and make sure the edge policies
		// are populated when a matched edge is found.
		for _, e := range chanGraph.Edges {
			if e.ChanPoint != cpStr {
				continue
			}

			if e.Node1Policy == nil {
				return fmt.Errorf("no policy for node1 %v",
					e.Node1Pub)
			}

			if e.Node2Policy == nil {
				return fmt.Errorf("no policy for node2 %v",
					e.Node1Pub)
			}

			edge = e

			return nil
		}

		// If we've iterated over all the known edges and we weren't
		// able to find this specific one, then we'll fail.
		return fmt.Errorf("no edge found for channel point: %s", cpStr)
	}, DefaultTimeout)

	require.NoError(h, err, "%s: timeout finding channel %v in graph cache",
		cpStr, hn.Name())

	return edge
}

// AssertChannelInGraphDB asserts that a given channel is found both in the
// graph db (GetChanInfo) and the graph cache (DescribeGraph).
func (h *HarnessTest) AssertChannelInGraph(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.ChannelEdge {

	// Make sure the channel is found in the db first.
	h.AssertChannelInGraphDB(hn, chanPoint)

	// Assert the channel is also found in the graph cache, which refreshes
	// every `--caches.rpc-graph-cache-duration`.
	return h.AssertChannelInGraphCache(hn, chanPoint)
}

// AssertTxAtHeight gets all of the transactions that a node's wallet has a
// record of at the target height, and finds and returns the tx with the target
// txid, failing if it is not found.
func (h *HarnessTest) AssertTxAtHeight(hn *node.HarnessNode, height int32,
	txid *chainhash.Hash) *lnrpc.Transaction {

	req := &lnrpc.GetTransactionsRequest{
		StartHeight: height,
		EndHeight:   height,
	}
	txns := hn.RPC.GetTransactions(req)

	for _, tx := range txns.Transactions {
		if tx.TxHash == txid.String() {
			return tx
		}
	}

	require.Failf(h, "fail to find tx", "tx:%v not found at height:%v",
		txid, height)

	return nil
}

// getChannelPolicies queries the channel graph and retrieves the current edge
// policies for the provided channel point.
func (h *HarnessTest) getChannelPolicies(hn *node.HarnessNode,
	advertisingNode string,
	cp *lnrpc.ChannelPoint) (*lnrpc.RoutingPolicy, error) {

	req := &lnrpc.ChannelGraphRequest{IncludeUnannounced: true}
	chanGraph := hn.RPC.DescribeGraph(req)

	cpStr := channelPointStr(cp)
	for _, e := range chanGraph.Edges {
		if e.ChanPoint != cpStr {
			continue
		}

		if e.Node1Pub == advertisingNode {
			return e.Node1Policy, nil
		}

		return e.Node2Policy, nil
	}

	// If we've iterated over all the known edges and we weren't
	// able to find this specific one, then we'll fail.
	return nil, fmt.Errorf("did not find edge with advertisingNode: %s"+
		", channel point: %s", advertisingNode, cpStr)
}

// AssertChannelPolicy asserts that the passed node's known channel policy for
// the passed chanPoint is consistent with the expected policy values.
func (h *HarnessTest) AssertChannelPolicy(hn *node.HarnessNode,
	advertisingNode string, expectedPolicy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint) {

	policy, err := h.getChannelPolicies(hn, advertisingNode, chanPoint)
	require.NoErrorf(h, err, "%s: failed to find policy", hn.Name())

	err = node.CheckChannelPolicy(policy, expectedPolicy)
	require.NoErrorf(h, err, "%s: check policy failed", hn.Name())
}

// AssertNumPolicyUpdates asserts that a given number of channel policy updates
// has been seen in the specified node.
func (h *HarnessTest) AssertNumPolicyUpdates(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint,
	advertisingNode *node.HarnessNode, num int) {

	op := h.OutPointFromChannelPoint(chanPoint)

	var policies []*node.PolicyUpdateInfo

	err := wait.NoError(func() error {
		policyMap := hn.Watcher.GetPolicyUpdates(op)
		nodePolicy, ok := policyMap[advertisingNode.PubKeyStr]
		if ok {
			policies = nodePolicy
		}

		if len(policies) == num {
			return nil
		}

		p, err := json.MarshalIndent(policies, "", "\t")
		require.NoError(h, err, "encode policy err")

		return fmt.Errorf("expected to find %d policy updates, "+
			"instead got: %d, chanPoint: %v, "+
			"advertisingNode: %s:%s, policy: %s", num,
			len(policies), op, advertisingNode.Name(),
			advertisingNode.PubKeyStr, p)
	}, DefaultTimeout)

	require.NoError(h, err, "%s: timeout waiting for num of policy updates",
		hn.Name())
}

// AssertNumPayments asserts that the number of payments made within the test
// scope is as expected, including the incomplete ones.
func (h *HarnessTest) AssertNumPayments(hn *node.HarnessNode,
	num int) []*lnrpc.Payment {

	// Get the number of payments we already have from the previous test.
	have := hn.State.Payment.Total

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
		IndexOffset:       hn.State.Payment.LastIndexOffset,
	}

	var payments []*lnrpc.Payment
	err := wait.NoError(func() error {
		resp := hn.RPC.ListPayments(req)

		payments = resp.Payments
		if len(payments) == num {
			return nil
		}

		return errNumNotMatched(hn.Name(), "num of payments",
			num, len(payments), have+len(payments), have)
	}, DefaultTimeout)
	require.NoError(h, err, "%s: timeout checking num of payments",
		hn.Name())

	return payments
}

// AssertNumNodeAnns asserts that a given number of node announcements has been
// seen in the specified node.
func (h *HarnessTest) AssertNumNodeAnns(hn *node.HarnessNode,
	pubkey string, num int) []*lnrpc.NodeUpdate {

	// We will get the current number of channel updates first and add it
	// to our expected number of newly created channel updates.
	anns, err := hn.Watcher.WaitForNumNodeUpdates(pubkey, num)
	require.NoError(h, err, "%s: failed to assert num of node anns",
		hn.Name())

	return anns
}

// AssertNumChannelUpdates asserts that a given number of channel updates has
// been seen in the specified node's network topology.
func (h *HarnessTest) AssertNumChannelUpdates(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, num int) {

	op := h.OutPointFromChannelPoint(chanPoint)
	err := hn.Watcher.WaitForNumChannelUpdates(op, num)
	require.NoError(h, err, "%s: failed to assert num of channel updates",
		hn.Name())
}

// CreateBurnAddr creates a random burn address of the given type.
func (h *HarnessTest) CreateBurnAddr(addrType lnrpc.AddressType) ([]byte,
	btcutil.Address) {

	randomPrivKey, err := btcec.NewPrivateKey()
	require.NoError(h, err)

	randomKeyBytes := randomPrivKey.PubKey().SerializeCompressed()
	harnessNetParams := miner.HarnessNetParams

	var addr btcutil.Address
	switch addrType {
	case lnrpc.AddressType_WITNESS_PUBKEY_HASH:
		addr, err = btcutil.NewAddressWitnessPubKeyHash(
			btcutil.Hash160(randomKeyBytes), harnessNetParams,
		)

	case lnrpc.AddressType_TAPROOT_PUBKEY:
		taprootKey := txscript.ComputeTaprootKeyNoScript(
			randomPrivKey.PubKey(),
		)
		addr, err = btcutil.NewAddressPubKey(
			schnorr.SerializePubKey(taprootKey), harnessNetParams,
		)

	case lnrpc.AddressType_NESTED_PUBKEY_HASH:
		var witnessAddr btcutil.Address
		witnessAddr, err = btcutil.NewAddressWitnessPubKeyHash(
			btcutil.Hash160(randomKeyBytes), harnessNetParams,
		)
		require.NoError(h, err)

		addr, err = btcutil.NewAddressScriptHash(
			h.PayToAddrScript(witnessAddr), harnessNetParams,
		)

	default:
		h.Fatalf("Unsupported burn address type: %v", addrType)
	}
	require.NoError(h, err)

	return h.PayToAddrScript(addr), addr
}

// ReceiveTrackPayment waits until a message is received on the track payment
// stream or the timeout is reached.
func (h *HarnessTest) ReceiveTrackPayment(
	stream rpc.TrackPaymentClient) *lnrpc.Payment {

	chanMsg := make(chan *lnrpc.Payment)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout trakcing payment")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// ReceiveHtlcEvent waits until a message is received on the subscribe
// htlc event stream or the timeout is reached.
func (h *HarnessTest) ReceiveHtlcEvent(
	stream rpc.HtlcEventsClient) *routerrpc.HtlcEvent {

	chanMsg := make(chan *routerrpc.HtlcEvent)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout receiving htlc "+
			"event update")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// AssertHtlcEventType consumes one event from a client and asserts the event
// type is matched.
func (h *HarnessTest) AssertHtlcEventType(client rpc.HtlcEventsClient,
	userType routerrpc.HtlcEvent_EventType) *routerrpc.HtlcEvent {

	event := h.ReceiveHtlcEvent(client)
	require.Equalf(h, userType, event.EventType, "wrong event type, "+
		"want %v got %v", userType, event.EventType)

	return event
}

// HtlcEvent maps the series of event types used in `*routerrpc.HtlcEvent_*`.
type HtlcEvent int

const (
	HtlcEventForward HtlcEvent = iota
	HtlcEventForwardFail
	HtlcEventSettle
	HtlcEventLinkFail
	HtlcEventFinal
)

// AssertHtlcEventType consumes one event from a client and asserts both the
// user event type the event.Event type is matched.
func (h *HarnessTest) AssertHtlcEventTypes(client rpc.HtlcEventsClient,
	userType routerrpc.HtlcEvent_EventType,
	eventType HtlcEvent) *routerrpc.HtlcEvent {

	event := h.ReceiveHtlcEvent(client)
	require.Equalf(h, userType, event.EventType, "wrong event type, "+
		"want %v got %v", userType, event.EventType)

	var ok bool

	switch eventType {
	case HtlcEventForward:
		_, ok = event.Event.(*routerrpc.HtlcEvent_ForwardEvent)

	case HtlcEventForwardFail:
		_, ok = event.Event.(*routerrpc.HtlcEvent_ForwardFailEvent)

	case HtlcEventSettle:
		_, ok = event.Event.(*routerrpc.HtlcEvent_SettleEvent)

	case HtlcEventLinkFail:
		_, ok = event.Event.(*routerrpc.HtlcEvent_LinkFailEvent)

	case HtlcEventFinal:
		_, ok = event.Event.(*routerrpc.HtlcEvent_FinalHtlcEvent)
	}

	require.Truef(h, ok, "wrong event type: %T, want %T", event.Event,
		eventType)

	return event
}

// AssertFeeReport checks that the fee report from the given node has the
// desired day, week, and month sum values.
func (h *HarnessTest) AssertFeeReport(hn *node.HarnessNode,
	day, week, month int) {

	err := wait.NoError(func() error {
		feeReport, err := hn.RPC.LN.FeeReport(
			h.runCtx, &lnrpc.FeeReportRequest{},
		)
		require.NoError(h, err, "unable to query for fee report")

		if uint64(day) != feeReport.DayFeeSum {
			return fmt.Errorf("day fee mismatch, want %d, got %d",
				day, feeReport.DayFeeSum)
		}

		if uint64(week) != feeReport.WeekFeeSum {
			return fmt.Errorf("week fee mismatch, want %d, got %d",
				week, feeReport.WeekFeeSum)
		}
		if uint64(month) != feeReport.MonthFeeSum {
			return fmt.Errorf("month fee mismatch, want %d, got %d",
				month, feeReport.MonthFeeSum)
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoErrorf(h, err, "%s: time out checking fee report", hn.Name())
}

// AssertHtlcEvents consumes events from a client and ensures that they are of
// the expected type and contain the expected number of forwards, forward
// failures and settles.
//
// TODO(yy): needs refactor to reduce its complexity.
func (h *HarnessTest) AssertHtlcEvents(client rpc.HtlcEventsClient,
	fwdCount, fwdFailCount, settleCount, linkFailCount int,
	userType routerrpc.HtlcEvent_EventType) []*routerrpc.HtlcEvent {

	var forwards, forwardFails, settles, linkFails int

	numEvents := fwdCount + fwdFailCount + settleCount + linkFailCount
	events := make([]*routerrpc.HtlcEvent, 0)

	// It's either the userType or the unknown type.
	//
	// TODO(yy): maybe the FinalHtlcEvent shouldn't be in UNKNOWN type?
	eventTypes := []routerrpc.HtlcEvent_EventType{
		userType, routerrpc.HtlcEvent_UNKNOWN,
	}

	for i := 0; i < numEvents; i++ {
		event := h.ReceiveHtlcEvent(client)

		require.Containsf(h, eventTypes, event.EventType,
			"wrong event type, want %v, got %v", userType,
			event.EventType)

		events = append(events, event)

		switch e := event.Event.(type) {
		case *routerrpc.HtlcEvent_ForwardEvent:
			forwards++

		case *routerrpc.HtlcEvent_ForwardFailEvent:
			forwardFails++

		case *routerrpc.HtlcEvent_SettleEvent:
			settles++

		case *routerrpc.HtlcEvent_FinalHtlcEvent:
			if e.FinalHtlcEvent.Settled {
				settles++
			}

		case *routerrpc.HtlcEvent_LinkFailEvent:
			linkFails++

		default:
			require.Fail(h, "assert event fail",
				"unexpected event: %T", event.Event)
		}
	}

	require.Equal(h, fwdCount, forwards, "num of forwards mismatch")
	require.Equal(h, fwdFailCount, forwardFails,
		"num of forward fails mismatch")
	require.Equal(h, settleCount, settles, "num of settles mismatch")
	require.Equal(h, linkFailCount, linkFails, "num of link fails mismatch")

	return events
}

// AssertTransactionInWallet asserts a given txid can be found in the node's
// wallet.
func (h *HarnessTest) AssertTransactionInWallet(hn *node.HarnessNode,
	txid chainhash.Hash) {

	req := &lnrpc.GetTransactionsRequest{}
	err := wait.NoError(func() error {
		txResp := hn.RPC.GetTransactions(req)
		for _, txn := range txResp.Transactions {
			if txn.TxHash == txid.String() {
				return nil
			}
		}

		return fmt.Errorf("%s: expected txid=%v not found in wallet",
			hn.Name(), txid)
	}, DefaultTimeout)

	require.NoError(h, err, "failed to find tx")
}

// AssertTransactionNotInWallet asserts a given txid can NOT be found in the
// node's wallet.
func (h *HarnessTest) AssertTransactionNotInWallet(hn *node.HarnessNode,
	txid chainhash.Hash) {

	req := &lnrpc.GetTransactionsRequest{}
	err := wait.NoError(func() error {
		txResp := hn.RPC.GetTransactions(req)
		for _, txn := range txResp.Transactions {
			if txn.TxHash == txid.String() {
				return fmt.Errorf("expected txid=%v to be "+
					"not found", txid)
			}
		}

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: failed to assert tx not found", hn.Name())
}

// WaitForNodeBlockHeight queries the node for its current block height until
// it reaches the passed height.
func (h *HarnessTest) WaitForNodeBlockHeight(hn *node.HarnessNode,
	height int32) {

	err := wait.NoError(func() error {
		info := hn.RPC.GetInfo()
		if int32(info.BlockHeight) != height {
			return fmt.Errorf("expected block height to "+
				"be %v, was %v", height, info.BlockHeight)
		}

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: timeout while waiting for height",
		hn.Name())
}

// AssertChannelCommitHeight asserts the given channel for the node has the
// expected commit height(`NumUpdates`).
func (h *HarnessTest) AssertChannelCommitHeight(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, height int) {

	err := wait.NoError(func() error {
		c, err := h.findChannel(hn, cp)
		if err != nil {
			return err
		}

		if int(c.NumUpdates) == height {
			return nil
		}

		return fmt.Errorf("expected commit height to be %v, was %v",
			height, c.NumUpdates)
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while waiting for commit height")
}

// AssertNumInvoices asserts that the number of invoices made within the test
// scope is as expected.
func (h *HarnessTest) AssertNumInvoices(hn *node.HarnessNode,
	num int) []*lnrpc.Invoice {

	have := hn.State.Invoice.Total
	req := &lnrpc.ListInvoiceRequest{
		NumMaxInvoices: math.MaxUint64,
		IndexOffset:    hn.State.Invoice.LastIndexOffset,
	}

	var invoices []*lnrpc.Invoice
	err := wait.NoError(func() error {
		resp := hn.RPC.ListInvoices(req)

		invoices = resp.Invoices
		if len(invoices) == num {
			return nil
		}

		return errNumNotMatched(hn.Name(), "num of invoices",
			num, len(invoices), have+len(invoices), have)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking num of invoices")

	return invoices
}

// ReceiveSendToRouteUpdate waits until a message is received on the
// SendToRoute client stream or the timeout is reached.
func (h *HarnessTest) ReceiveSendToRouteUpdate(
	stream rpc.SendToRouteClient) (*lnrpc.SendResponse, error) {

	chanMsg := make(chan *lnrpc.SendResponse, 1)
	errChan := make(chan error, 1)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err

			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout waiting for send resp")
		return nil, nil

	case err := <-errChan:
		return nil, err

	case updateMsg := <-chanMsg:
		return updateMsg, nil
	}
}

// AssertInvoiceEqual asserts that two lnrpc.Invoices are equivalent. A custom
// comparison function is defined for these tests, since proto message returned
// from unary and streaming RPCs (as of protobuf 1.23.0 and grpc 1.29.1) aren't
// consistent with the private fields set on the messages. As a result, we
// avoid using require.Equal and test only the actual data members.
func (h *HarnessTest) AssertInvoiceEqual(a, b *lnrpc.Invoice) {
	// Ensure the HTLCs are sorted properly before attempting to compare.
	sort.Slice(a.Htlcs, func(i, j int) bool {
		return a.Htlcs[i].ChanId < a.Htlcs[j].ChanId
	})
	sort.Slice(b.Htlcs, func(i, j int) bool {
		return b.Htlcs[i].ChanId < b.Htlcs[j].ChanId
	})

	require.Equal(h, a.Memo, b.Memo)
	require.Equal(h, a.RPreimage, b.RPreimage)
	require.Equal(h, a.RHash, b.RHash)
	require.Equal(h, a.Value, b.Value)
	require.Equal(h, a.ValueMsat, b.ValueMsat)
	require.Equal(h, a.CreationDate, b.CreationDate)
	require.Equal(h, a.SettleDate, b.SettleDate)
	require.Equal(h, a.PaymentRequest, b.PaymentRequest)
	require.Equal(h, a.DescriptionHash, b.DescriptionHash)
	require.Equal(h, a.Expiry, b.Expiry)
	require.Equal(h, a.FallbackAddr, b.FallbackAddr)
	require.Equal(h, a.CltvExpiry, b.CltvExpiry)
	require.Equal(h, a.RouteHints, b.RouteHints)
	require.Equal(h, a.Private, b.Private)
	require.Equal(h, a.AddIndex, b.AddIndex)
	require.Equal(h, a.SettleIndex, b.SettleIndex)
	require.Equal(h, a.AmtPaidSat, b.AmtPaidSat)
	require.Equal(h, a.AmtPaidMsat, b.AmtPaidMsat)
	require.Equal(h, a.State, b.State)
	require.Equal(h, a.Features, b.Features)
	require.Equal(h, a.IsKeysend, b.IsKeysend)
	require.Equal(h, a.PaymentAddr, b.PaymentAddr)
	require.Equal(h, a.IsAmp, b.IsAmp)

	require.Equal(h, len(a.Htlcs), len(b.Htlcs))
	for i := range a.Htlcs {
		htlcA, htlcB := a.Htlcs[i], b.Htlcs[i]
		require.Equal(h, htlcA.ChanId, htlcB.ChanId)
		require.Equal(h, htlcA.HtlcIndex, htlcB.HtlcIndex)
		require.Equal(h, htlcA.AmtMsat, htlcB.AmtMsat)
		require.Equal(h, htlcA.AcceptHeight, htlcB.AcceptHeight)
		require.Equal(h, htlcA.AcceptTime, htlcB.AcceptTime)
		require.Equal(h, htlcA.ResolveTime, htlcB.ResolveTime)
		require.Equal(h, htlcA.ExpiryHeight, htlcB.ExpiryHeight)
		require.Equal(h, htlcA.State, htlcB.State)
		require.Equal(h, htlcA.CustomRecords, htlcB.CustomRecords)
		require.Equal(h, htlcA.MppTotalAmtMsat, htlcB.MppTotalAmtMsat)
		require.Equal(h, htlcA.Amp, htlcB.Amp)
	}
}

// AssertUTXOInWallet asserts that a given UTXO can be found in the node's
// wallet.
func (h *HarnessTest) AssertUTXOInWallet(hn *node.HarnessNode,
	op *lnrpc.OutPoint, account string) {

	err := wait.NoError(func() error {
		utxos := h.GetUTXOs(hn, account)

		err := fmt.Errorf("tx with hash %x not found", op.TxidBytes)
		for _, utxo := range utxos {
			if !bytes.Equal(utxo.Outpoint.TxidBytes, op.TxidBytes) {
				continue
			}

			err = fmt.Errorf("tx with output index %v not found",
				op.OutputIndex)
			if utxo.Outpoint.OutputIndex != op.OutputIndex {
				continue
			}

			return nil
		}

		return err
	}, DefaultTimeout)

	require.NoErrorf(h, err, "outpoint %v not found in %s's wallet",
		op, hn.Name())
}

// AssertWalletAccountBalance asserts that the unconfirmed and confirmed
// balance for the given account is satisfied by the WalletBalance and
// ListUnspent RPCs. The unconfirmed balance is not checked for neutrino nodes.
func (h *HarnessTest) AssertWalletAccountBalance(hn *node.HarnessNode,
	account string, confirmedBalance, unconfirmedBalance int64) {

	err := wait.NoError(func() error {
		balanceResp := hn.RPC.WalletBalance()
		require.Contains(h, balanceResp.AccountBalance, account)
		accountBalance := balanceResp.AccountBalance[account]

		// Check confirmed balance.
		if accountBalance.ConfirmedBalance != confirmedBalance {
			return fmt.Errorf("expected confirmed balance %v, "+
				"got %v", confirmedBalance,
				accountBalance.ConfirmedBalance)
		}

		utxos := h.GetUTXOsConfirmed(hn, account)
		var totalConfirmedVal int64
		for _, utxo := range utxos {
			totalConfirmedVal += utxo.AmountSat
		}
		if totalConfirmedVal != confirmedBalance {
			return fmt.Errorf("expected total confirmed utxo "+
				"balance %v, got %v", confirmedBalance,
				totalConfirmedVal)
		}

		// Skip unconfirmed balance checks for neutrino nodes.
		if h.IsNeutrinoBackend() {
			return nil
		}

		// Check unconfirmed balance.
		if accountBalance.UnconfirmedBalance != unconfirmedBalance {
			return fmt.Errorf("expected unconfirmed balance %v, "+
				"got %v", unconfirmedBalance,
				accountBalance.UnconfirmedBalance)
		}

		utxos = h.GetUTXOsUnconfirmed(hn, account)
		var totalUnconfirmedVal int64
		for _, utxo := range utxos {
			totalUnconfirmedVal += utxo.AmountSat
		}
		if totalUnconfirmedVal != unconfirmedBalance {
			return fmt.Errorf("expected total unconfirmed utxo "+
				"balance %v, got %v", unconfirmedBalance,
				totalUnconfirmedVal)
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking wallet account balance")
}

// AssertClosingTxInMempool assert that the closing transaction of the given
// channel point can be found in the mempool. If the channel has anchors, it
// will assert the anchor sweep tx is also in the mempool.
func (h *HarnessTest) AssertClosingTxInMempool(cp *lnrpc.ChannelPoint,
	c lnrpc.CommitmentType) *wire.MsgTx {

	// Get expected number of txes to be found in the mempool.
	expectedTxes := 1
	hasAnchors := CommitTypeHasAnchors(c)
	if hasAnchors {
		expectedTxes = 2
	}

	// Wait for the expected txes to be found in the mempool.
	h.AssertNumTxsInMempool(expectedTxes)

	// Get the closing tx from the mempool.
	op := h.OutPointFromChannelPoint(cp)
	closeTx := h.AssertOutpointInMempool(op)

	return closeTx
}

// AssertClosingTxInMempool assert that the closing transaction of the given
// channel point can be found in the mempool. If the channel has anchors, it
// will assert the anchor sweep tx is also in the mempool.
func (h *HarnessTest) MineClosingTx(cp *lnrpc.ChannelPoint) *wire.MsgTx {
	// Wait for the expected txes to be found in the mempool.
	h.AssertNumTxsInMempool(1)

	// Get the closing tx from the mempool.
	op := h.OutPointFromChannelPoint(cp)
	closeTx := h.AssertOutpointInMempool(op)

	// Mine a block to confirm the closing transaction and potential anchor
	// sweep.
	h.MineBlocksAndAssertNumTxes(1, 1)

	return closeTx
}

// AssertWalletLockedBalance asserts the expected amount has been marked as
// locked in the node's WalletBalance response.
func (h *HarnessTest) AssertWalletLockedBalance(hn *node.HarnessNode,
	balance int64) {

	err := wait.NoError(func() error {
		balanceResp := hn.RPC.WalletBalance()
		got := balanceResp.LockedBalance

		if got != balance {
			return fmt.Errorf("want %d, got %d", balance, got)
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(h, err, "%s: timeout checking locked balance",
		hn.Name())
}

// AssertNumPendingSweeps asserts the number of pending sweeps for the given
// node.
func (h *HarnessTest) AssertNumPendingSweeps(hn *node.HarnessNode,
	n int) []*walletrpc.PendingSweep {

	results := make([]*walletrpc.PendingSweep, 0, n)

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingSweeps()
		num := len(resp.PendingSweeps)

		numDesc := "\n"
		for _, s := range resp.PendingSweeps {
			desc := fmt.Sprintf("op=%v:%v, amt=%v, type=%v, "+
				"deadline=%v, maturityHeight=%v\n",
				s.Outpoint.TxidStr, s.Outpoint.OutputIndex,
				s.AmountSat, s.WitnessType, s.DeadlineHeight,
				s.MaturityHeight)
			numDesc += desc

			// The deadline height must be set, otherwise the
			// pending input response is not update-to-date.
			if s.DeadlineHeight == 0 {
				return fmt.Errorf("input not updated: %s", desc)
			}
		}

		if num == n {
			results = resp.PendingSweeps
			return nil
		}

		return fmt.Errorf("want %d , got %d, sweeps: %s", n, num,
			numDesc)
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: check pending sweeps timeout", hn.Name())

	return results
}

// AssertAtLeastNumPendingSweeps asserts there are at least n pending sweeps for
// the given node.
func (h *HarnessTest) AssertAtLeastNumPendingSweeps(hn *node.HarnessNode,
	n int) []*walletrpc.PendingSweep {

	results := make([]*walletrpc.PendingSweep, 0, n)

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingSweeps()
		num := len(resp.PendingSweeps)

		numDesc := "\n"
		for _, s := range resp.PendingSweeps {
			desc := fmt.Sprintf("op=%v:%v, amt=%v, type=%v, "+
				"deadline=%v, maturityHeight=%v\n",
				s.Outpoint.TxidStr, s.Outpoint.OutputIndex,
				s.AmountSat, s.WitnessType, s.DeadlineHeight,
				s.MaturityHeight)
			numDesc += desc

			// The deadline height must be set, otherwise the
			// pending input response is not update-to-date.
			if s.DeadlineHeight == 0 {
				return fmt.Errorf("input not updated: %s", desc)
			}
		}

		if num >= n {
			results = resp.PendingSweeps
			return nil
		}

		return fmt.Errorf("want %d , got %d, sweeps: %s", n, num,
			numDesc)
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: check pending sweeps timeout", hn.Name())

	return results
}

// AssertNumSweeps asserts the number of sweeps for the given node.
func (h *HarnessTest) AssertNumSweeps(hn *node.HarnessNode,
	req *walletrpc.ListSweepsRequest,
	n int) *walletrpc.ListSweepsResponse {

	var result *walletrpc.ListSweepsResponse

	// The ListSweeps call is wrapped in wait.NoError to handle potential
	// timing issues. Sweep transactions might not be immediately reflected
	// or processed by the node after an event (e.g., channel closure or
	// block mining) due to propagation or processing delays. This ensures
	// the system retries the call until the expected sweep is found,
	// preventing test flakes caused by race conditions.
	err := wait.NoError(func() error {
		resp := hn.RPC.ListSweeps(req)

		var txIDs []string
		if req.Verbose {
			details := resp.GetTransactionDetails()
			if details != nil {
				for _, tx := range details.Transactions {
					txIDs = append(txIDs, tx.TxHash)
				}
			}
		} else {
			ids := resp.GetTransactionIds()
			if ids != nil {
				txIDs = ids.TransactionIds
			}
		}

		num := len(txIDs)

		// Exit early if the num matches.
		if num == n {
			result = resp
			return nil
		}

		return fmt.Errorf("want %d, got %d, sweeps: %v, req: %v", n,
			num, txIDs, req)
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: check num of sweeps timeout", hn.Name())

	return result
}

// FindSweepingTxns asserts the expected number of sweeping txns are found in
// the txns specified and return them.
func (h *HarnessTest) FindSweepingTxns(txns []*wire.MsgTx,
	expectedNumSweeps int, closeTxid chainhash.Hash) []*wire.MsgTx {

	var sweepTxns []*wire.MsgTx

	for _, tx := range txns {
		if tx.TxIn[0].PreviousOutPoint.Hash == closeTxid {
			sweepTxns = append(sweepTxns, tx)
		}
	}
	require.Len(h, sweepTxns, expectedNumSweeps, "unexpected num of sweeps")

	return sweepTxns
}

// AssertForceCloseAndAnchorTxnsInMempool asserts that the force close and
// anchor sweep txns are found in the mempool and returns the force close tx
// and the anchor sweep tx.
func (h *HarnessTest) AssertForceCloseAndAnchorTxnsInMempool() (*wire.MsgTx,
	*wire.MsgTx) {

	// Assert there are two txns in the mempool.
	txns := h.GetNumTxsFromMempool(2)

	// isParentAndChild checks whether there is an input used in the
	// assumed child tx by checking every input's previous outpoint against
	// the assumed parentTxid.
	isParentAndChild := func(parent, child *wire.MsgTx) bool {
		parentTxid := parent.TxHash()

		for _, inp := range child.TxIn {
			if inp.PreviousOutPoint.Hash == parentTxid {
				// Found a match, this is indeed the anchor
				// sweeping tx so we return it here.
				return true
			}
		}

		return false
	}

	switch {
	// Assume the first one is the closing tx and the second one is the
	// anchor sweeping tx.
	case isParentAndChild(txns[0], txns[1]):
		return txns[0], txns[1]

	// Assume the first one is the anchor sweeping tx and the second one is
	// the closing tx.
	case isParentAndChild(txns[1], txns[0]):
		return txns[1], txns[0]

	// Unrelated txns found, fail the test.
	default:
		h.Fatalf("the two txns not related: %v", txns)

		return nil, nil
	}
}

// ReceiveSendToRouteUpdate waits until a message is received on the
// PeerEventsClient stream or the timeout is reached.
func (h *HarnessTest) ReceivePeerEvent(
	stream rpc.PeerEventsClient) (*lnrpc.PeerEvent, error) {

	eventChan := make(chan *lnrpc.PeerEvent, 1)
	errChan := make(chan error, 1)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err

			return
		}
		eventChan <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout waiting for peer event")
		return nil, nil

	case err := <-errChan:
		return nil, err

	case event := <-eventChan:
		return event, nil
	}
}

// AssertPeerOnlineEvent reads an event from the PeerEventsClient stream and
// asserts it's an online event.
func (h HarnessTest) AssertPeerOnlineEvent(stream rpc.PeerEventsClient) {
	event, err := h.ReceivePeerEvent(stream)
	require.NoError(h, err)

	require.Equal(h, lnrpc.PeerEvent_PEER_ONLINE, event.Type)
}

// AssertPeerOfflineEvent reads an event from the PeerEventsClient stream and
// asserts it's an offline event.
func (h HarnessTest) AssertPeerOfflineEvent(stream rpc.PeerEventsClient) {
	event, err := h.ReceivePeerEvent(stream)
	require.NoError(h, err)

	require.Equal(h, lnrpc.PeerEvent_PEER_OFFLINE, event.Type)
}

// AssertPeerReconnected reads two events from the PeerEventsClient stream. The
// first event must be an offline event, and the second event must be an online
// event. This is a typical reconnection scenario, where the peer is
// disconnected then connected again.
//
// NOTE: It's important to make the subscription before the disconnection
// happens, otherwise the events can be missed.
func (h HarnessTest) AssertPeerReconnected(stream rpc.PeerEventsClient) {
	h.AssertPeerOfflineEvent(stream)
	h.AssertPeerOnlineEvent(stream)
}
