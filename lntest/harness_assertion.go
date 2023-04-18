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
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
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
	bestBlock *wire.MsgBlock) {

	bestBlockHash := bestBlock.BlockHash().String()
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
	h.AssertPeerConnected(a, b)
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
		chanGraph := hn.RPC.DescribeGraph(req)
		total := len(chanGraph.Edges)

		if total-old == expected {
			if expected != 0 {
				// NOTE: assume edges come in ascending order
				// that the old edges are at the front of the
				// slice.
				edges = chanGraph.Edges[old:]
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

// AssertTopologyChannelOpen asserts that a given channel outpoint is seen by
// the passed node's network topology.
func (h *HarnessTest) AssertTopologyChannelOpen(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) {

	err := hn.Watcher.WaitForChannelOpen(chanPoint)
	require.NoErrorf(h, err, "%s didn't report channel", hn.Name())
}

// AssertChannelExists asserts that an active channel identified by the
// specified channel point exists from the point-of-view of the node.
func (h *HarnessTest) AssertChannelExists(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint) *lnrpc.Channel {

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
		if channel.Active {
			return nil
		}

		return fmt.Errorf("channel point not active")
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: timeout checking for channel point: %v",
		hn.Name(), cp)

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
	require.Equal(h, pkScript.Class(), scriptClass)
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

	return nil, fmt.Errorf("channel not found using %s", chanPoint)
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
	stream rpc.CloseChanClient) *chainhash.Hash {

	// Consume one event.
	event, err := h.ReceiveCloseChannelUpdate(stream)
	require.NoError(h, err)

	resp, ok := event.Update.(*lnrpc.CloseStatusUpdate_ChanClose)
	require.Truef(h, ok, "expected channel open update, instead got %v",
		resp)

	txid, err := chainhash.NewHash(resp.ChanClose.ClosingTxid)
	require.NoErrorf(h, err, "wrong format found in closing txid: %v",
		resp.ChanClose.ClosingTxid)

	return txid
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
	stream rpc.CloseChanClient) *chainhash.Hash {

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
	h.Miner.AssertTxInBlock(block, closingTxid)

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
// - assert the channel is waiting close and has the expected ChanStatusFlags.
// - assert the mempool has the closing txes and anchor sweeps.
// - mine a block and assert the closing txid is mined.
// - assert the channel is pending force close.
// - assert the node has seen the channel close update.
func (h *HarnessTest) AssertStreamChannelForceClosed(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, anchors bool,
	stream rpc.CloseChanClient) *chainhash.Hash {

	// Assert the channel is waiting close.
	resp := h.AssertChannelWaitingClose(hn, cp)

	// Assert that the channel is in local force broadcasted.
	require.Contains(h, resp.Channel.ChanStatusFlags,
		channeldb.ChanStatusLocalCloseInitiator.String(),
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
	h.Miner.AssertTxInBlock(block, closingTxid)

	// We should see zero waiting close channels and 1 pending force close
	// channels now.
	h.AssertNumWaitingClose(hn, 0)
	h.AssertNumPendingForceClose(hn, 1)

	// Finally, check that the node's topology graph has seen this channel
	// closed if it's a public channel.
	if !resp.Channel.Private {
		h.AssertTopologyChannelClosed(hn, cp)
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
//
// NOTE: for standby nodes(Alice and Bob), this method takes account of the
// previous state of the node's UTXOs. The previous state is snapshotted when
// finishing a previous test case via the cleanup function in `Subtest`. In
// other words, this assertion only checks the new changes made in the current
// test.
func (h *HarnessTest) AssertNumUTXOsWithConf(hn *node.HarnessNode,
	expectedUtxos int, max, min int32) []*lnrpc.Utxo {

	var unconfirmed bool

	old := hn.State.UTXO.Confirmed
	if max == 0 {
		old = hn.State.UTXO.Unconfirmed
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

		if total-old == expectedUtxos {
			utxos = resp.Utxos[old:]

			return nil
		}

		return errNumNotMatched(hn.Name(), "num of UTXOs",
			expectedUtxos, total-old, total, old)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout waiting for UTXOs")

	return utxos
}

// AssertNumUTXOsUnconfirmed asserts the expected num of unconfirmed utxos are
// seen.
//
// NOTE: for standby nodes(Alice and Bob), this method takes account of the
// previous state of the node's UTXOs. Check `AssertNumUTXOsWithConf` for
// details.
func (h *HarnessTest) AssertNumUTXOsUnconfirmed(hn *node.HarnessNode,
	num int) []*lnrpc.Utxo {

	return h.AssertNumUTXOsWithConf(hn, num, 0, 0)
}

// AssertNumUTXOsConfirmed asserts the expected num of confirmed utxos are
// seen, which means the returned utxos have at least one confirmation.
//
// NOTE: for standby nodes(Alice and Bob), this method takes account of the
// previous state of the node's UTXOs. Check `AssertNumUTXOsWithConf` for
// details.
func (h *HarnessTest) AssertNumUTXOsConfirmed(hn *node.HarnessNode,
	num int) []*lnrpc.Utxo {

	return h.AssertNumUTXOsWithConf(hn, num, math.MaxInt32, 1)
}

// AssertNumUTXOs asserts the expected num of utxos are seen, including
// confirmed and unconfirmed outputs.
//
// NOTE: for standby nodes(Alice and Bob), this method takes account of the
// previous state of the node's UTXOs. Check `AssertNumUTXOsWithConf` for
// details.
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
	resp, err := btcutil.DecodeAddress(addr, harnessNetParams)
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
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		req := &lnrpc.ListChannelsRequest{}
		nodeChans := hn.RPC.ListChannels(req)

		total := 0
		for _, channel := range nodeChans.Channels {
			total += len(channel.PendingHtlcs)
		}
		if total-old != num {
			return errNumNotMatched(hn.Name(), "active HTLCs",
				num, total-old, total, old)
		}

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s timeout checking num active htlcs",
		hn.Name())
}

// AssertActiveHtlcs makes sure the node has the _exact_ HTLCs matching
// payHashes on _all_ their channels.
func (h *HarnessTest) AssertActiveHtlcs(hn *node.HarnessNode,
	payHashes ...[]byte) {

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		req := &lnrpc.ListChannelsRequest{}
		nodeChans := hn.RPC.ListChannels(req)

		for _, ch := range nodeChans.Channels {
			// Record all payment hashes active for this channel.
			htlcHashes := make(map[string]struct{})

			for _, htlc := range ch.PendingHtlcs {
				h := hex.EncodeToString(htlc.HashLock)
				_, ok := htlcHashes[h]
				if ok {
					return fmt.Errorf("duplicate HashLock "+
						"in PendingHtlcs: %v",
						ch.PendingHtlcs)
				}
				htlcHashes[h] = struct{}{}
			}

			// Channel should have exactly the payHashes active.
			if len(payHashes) != len(htlcHashes) {
				return fmt.Errorf("node [%s:%x] had %v "+
					"htlcs active, expected %v",
					hn.Name(), hn.PubKey[:],
					len(htlcHashes), len(payHashes))
			}

			// Make sure all the payHashes are active.
			for _, payHash := range payHashes {
				h := hex.EncodeToString(payHash)
				if _, ok := htlcHashes[h]; ok {
					continue
				}

				return fmt.Errorf("node [%s:%x] didn't have: "+
					"the payHash %v active", hn.Name(),
					hn.PubKey[:], h)
			}
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking active HTLCs")
}

// AssertIncomingHTLCActive asserts the node has a pending incoming HTLC in the
// given channel. Returns the HTLC if found and active.
func (h *HarnessTest) AssertIncomingHTLCActive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, payHash []byte) *lnrpc.HTLC {

	return h.assertHLTCActive(hn, cp, payHash, true)
}

// AssertOutgoingHTLCActive asserts the node has a pending outgoing HTLC in the
// given channel. Returns the HTLC if found and active.
func (h *HarnessTest) AssertOutgoingHTLCActive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, payHash []byte) *lnrpc.HTLC {

	return h.assertHLTCActive(hn, cp, payHash, false)
}

// assertHLTCActive asserts the node has a pending HTLC in the given channel.
// Returns the HTLC if found and active.
func (h *HarnessTest) assertHLTCActive(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, payHash []byte, incoming bool) *lnrpc.HTLC {

	var result *lnrpc.HTLC
	target := hex.EncodeToString(payHash)

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		ch := h.GetChannelByChanPoint(hn, cp)

		// Check all payment hashes active for this channel.
		for _, htlc := range ch.PendingHtlcs {
			h := hex.EncodeToString(htlc.HashLock)
			if h != target {
				continue
			}

			// If the payment hash is found, check the incoming
			// field.
			if htlc.Incoming == incoming {
				// Found it and return.
				result = htlc
				return nil
			}

			// Otherwise we do have the HTLC but its direction is
			// not right.
			have, want := "outgoing", "incoming"
			if htlc.Incoming {
				have, want = "incoming", "outgoing"
			}

			return fmt.Errorf("node[%s] have htlc(%v), want: %s, "+
				"have: %s", hn.Name(), payHash, want, have)
		}

		return fmt.Errorf("node [%s:%x] didn't have: the payHash %v",
			hn.Name(), hn.PubKey[:], payHash)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking pending HTLC")

	return result
}

// AssertHLTCNotActive asserts the node doesn't have a pending HTLC in the
// given channel, which mean either the HTLC never exists, or it was pending
// and now settled. Returns the HTLC if found and active.
//
// NOTE: to check a pending HTLC becoming settled, first use AssertHLTCActive
// then follow this check.
func (h *HarnessTest) AssertHLTCNotActive(hn *node.HarnessNode,
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
			return fmt.Errorf("got %d pending htlcs, want %d",
				len(target.PendingHtlcs), num)
		}

		for i, htlc := range target.PendingHtlcs {
			if htlc.Stage == stage {
				continue
			}

			return fmt.Errorf("HTLC %d got stage: %v, "+
				"want stage: %v", i, htlc.Stage, stage)
		}

		return nil
	}

	require.NoErrorf(h, wait.NoError(checkStage, DefaultTimeout),
		"timeout waiting for htlc stage")
}

// findPayment queries the payment from the node's ListPayments which matches
// the specified preimage hash.
func (h *HarnessTest) findPayment(hn *node.HarnessNode,
	paymentHash string) *lnrpc.Payment {

	req := &lnrpc.ListPaymentsRequest{IncludeIncomplete: true}
	paymentsResp := hn.RPC.ListPayments(req)

	for _, p := range paymentsResp.Payments {
		if p.PaymentHash != paymentHash {
			continue
		}

		return p
	}

	require.Failf(h, "payment not found", "payment %v cannot be found",
		paymentHash)

	return nil
}

// AssertPaymentStatus asserts that the given node list a payment with the
// given preimage has the expected status. It also checks that the payment has
// the expected preimage, which is empty when it's not settled and matches the
// given preimage when it's succeeded.
func (h *HarnessTest) AssertPaymentStatus(hn *node.HarnessNode,
	preimage lntypes.Preimage,
	status lnrpc.Payment_PaymentStatus) *lnrpc.Payment {

	var target *lnrpc.Payment
	payHash := preimage.Hash()

	err := wait.NoError(func() error {
		p := h.findPayment(hn, payHash.String())
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
		require.Equal(h, preimage.String(), target.PaymentPreimage,
			"preimage not match")

	// Otherwise we expect an all-zero preimage.
	default:
		require.Equal(h, (lntypes.Preimage{}).String(),
			target.PaymentPreimage, "expected zero preimage")
	}

	return target
}

// AssertActiveNodesSynced asserts all active nodes have synced to the chain.
func (h *HarnessTest) AssertActiveNodesSynced() {
	for _, node := range h.manager.activeNodes {
		h.WaitForBlockchainSync(node)
	}
}

// AssertActiveNodesSyncedTo asserts all active nodes have synced to the
// provided bestBlock.
func (h *HarnessTest) AssertActiveNodesSyncedTo(bestBlock *wire.MsgBlock) {
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

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	feeReport, err := hn.RPC.LN.FeeReport(ctxt, &lnrpc.FeeReportRequest{})
	require.NoError(h, err, "unable to query for fee report")

	require.EqualValues(h, day, feeReport.DayFeeSum, "day fee mismatch")
	require.EqualValues(h, week, feeReport.WeekFeeSum, "day week mismatch")
	require.EqualValues(h, month, feeReport.MonthFeeSum,
		"day month mismatch")
}

// AssertHtlcEvents consumes events from a client and ensures that they are of
// the expected type and contain the expected number of forwards, forward
// failures and settles.
//
// TODO(yy): needs refactor to reduce its complexity.
func (h *HarnessTest) AssertHtlcEvents(client rpc.HtlcEventsClient,
	fwdCount, fwdFailCount, settleCount int,
	userType routerrpc.HtlcEvent_EventType) []*routerrpc.HtlcEvent {

	var forwards, forwardFails, settles int

	numEvents := fwdCount + fwdFailCount + settleCount
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
			"wrong event type, got %v", userType, event.EventType)

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

		default:
			require.Fail(h, "assert event fail",
				"unexpected event: %T", event.Event)
		}
	}

	require.Equal(h, fwdCount, forwards, "num of forwards mismatch")
	require.Equal(h, fwdFailCount, forwardFails,
		"num of forward fails mismatch")
	require.Equal(h, settleCount, settles, "num of settles mismatch")

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
	h.Miner.AssertNumTxsInMempool(expectedTxes)

	// Get the closing tx from the mempool.
	op := h.OutPointFromChannelPoint(cp)
	closeTx := h.Miner.AssertOutpointInMempool(op)

	return closeTx
}

// AssertClosingTxInMempool assert that the closing transaction of the given
// channel point can be found in the mempool. If the channel has anchors, it
// will assert the anchor sweep tx is also in the mempool.
func (h *HarnessTest) MineClosingTx(cp *lnrpc.ChannelPoint,
	c lnrpc.CommitmentType) *wire.MsgTx {

	// Get expected number of txes to be found in the mempool.
	expectedTxes := 1
	hasAnchors := CommitTypeHasAnchors(c)
	if hasAnchors {
		expectedTxes = 2
	}

	// Wait for the expected txes to be found in the mempool.
	h.Miner.AssertNumTxsInMempool(expectedTxes)

	// Get the closing tx from the mempool.
	op := h.OutPointFromChannelPoint(cp)
	closeTx := h.Miner.AssertOutpointInMempool(op)

	// Mine a block to confirm the closing transaction and potential anchor
	// sweep.
	h.MineBlocksAndAssertNumTxes(1, expectedTxes)

	return closeTx
}
