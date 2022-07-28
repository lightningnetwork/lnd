package lntemp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntemp/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

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

// DisconnectNodes disconnects the given two nodes and asserts the
// disconnection is succeeded. The request is made from node a and sent to node
// b.
func (h *HarnessTest) DisconnectNodes(a, b *node.HarnessNode) {
	bobInfo := b.RPC.GetInfo()
	a.RPC.DisconnectPeer(bobInfo.IdentityPubkey)
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

		// If the connection is in process, we return no error.
		if strings.Contains(err.Error(), errConnectionRequested) {
			return nil
		}

		// If the two are already connected, we return early with no
		// error.
		if strings.Contains(err.Error(), "already connected to peer") {
			return nil
		}

		// We may get connection refused error if we happens to be in
		// the middle of a previous node disconnection, e.g., a restart
		// from one of the nodes.
		if strings.Contains(err.Error(), "connection refused") {
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

	case err := <-errChan:
		require.Failf(h, "open channel stream",
			"received err from open channel stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
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

// findChannel tries to find a target channel in the node using the given
// channel point.
func (h *HarnessTest) findChannel(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) (*lnrpc.Channel, error) {

	// Get the funding point.
	fp := h.OutPointFromChannelPoint(chanPoint)

	req := &lnrpc.ListChannelsRequest{}
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
		resp := hn.RPC.PendingChannels()
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

// assertChannelCoopClosed asserts that the channel is properly cleaned up
// after initiating a cooperative close.
func (h *HarnessTest) assertChannelCoopClosed(hn *node.HarnessNode,
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
	block := h.Miner.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]

	// Consume one close event and assert the closing txid can be found in
	// the block.
	closingTxid := h.WaitForChannelCloseEvent(stream)
	h.Miner.AssertTxInBlock(block, closingTxid)

	// We should see zero waiting close channels now.
	h.AssertNumWaitingClose(hn, 0)

	// Finally, check that the node's topology graph has seen this channel
	// closed.
	h.AssertTopologyChannelClosed(hn, cp)

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
	block := h.Miner.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]

	// Consume one close event and assert the closing txid can be found in
	// the block.
	closingTxid := h.WaitForChannelCloseEvent(stream)
	h.Miner.AssertTxInBlock(block, closingTxid)

	// We should see zero waiting close channels and 1 pending force close
	// channels now.
	h.AssertNumWaitingClose(hn, 0)
	h.AssertNumPendingForceClose(hn, 1)

	// Finally, check that the node's topology graph has seen this channel
	// closed.
	h.AssertTopologyChannelClosed(hn, cp)

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

	return h.assertPaymentStatusWithTimeout(stream, status, DefaultTimeout)
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
		payment := h.ReceivePaymentUpdate(stream)

		// Return if the desired payment state is reached.
		if payment.Status == status {
			target = payment

			return nil
		}

		// Return the err so that it can be used for debugging when
		// timeout is reached.
		return fmt.Errorf("payment status, got %v, want %v",
			payment.Status, status)
	}, timeout)

	require.NoError(h, err, "timeout while waiting payment")

	return target
}

// ReceivePaymentUpdate waits until a message is received on the payment client
// stream or the timeout is reached.
func (h *HarnessTest) ReceivePaymentUpdate(
	stream rpc.PaymentClient) *lnrpc.Payment {

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
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout waiting for payment update")

	case err := <-errChan:
		require.Failf(h, "payment stream",
			"received err from payment stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
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
					return fmt.Errorf("duplicate HashLock")
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
