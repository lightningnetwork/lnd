package lntemp

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
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

// ReceiveCloseChannelUpdate waits until a message is received on the subscribe
// channel close stream or the timeout is reached.
func (h *HarnessTest) ReceiveCloseChannelUpdate(
	stream rpc.CloseChanClient) *lnrpc.CloseStatusUpdate {

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

	case err := <-errChan:
		require.Failf(h, "close channel stream",
			"received err from close channel stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
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
	event := h.ReceiveCloseChannelUpdate(stream)

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
func (h *HarnessTest) AssertNumWaitingClose(hn *node.HarnessNode, num int) {
	oldWaiting := hn.State.CloseChannel.WaitingClose

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()
		total := len(resp.WaitingCloseChannels)

		got := total - oldWaiting
		if got == num {
			return nil
		}

		return errNumNotMatched(hn.Name(), "waiting close channels",
			num, got, total, oldWaiting)
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: assert waiting close timeout",
		hn.Name())
}

// AssertNumPendingForceClose checks that a PendingChannels response from the
// node reports the expected number of pending force close channels.
func (h *HarnessTest) AssertNumPendingForceClose(hn *node.HarnessNode,
	num int) {

	oldForce := hn.State.CloseChannel.PendingForceClose

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()
		total := len(resp.PendingForceClosingChannels)

		got := total - oldForce
		if got == num {
			return nil
		}

		return errNumNotMatched(hn.Name(), "pending force close "+
			"channels", num, got, total, oldForce)
	}, DefaultTimeout)

	require.NoErrorf(h, err, "%s: assert pending force close timeout",
		hn.Name())
}

// assertChannelClosed asserts that the channel is properly cleaned up after
// initiating a cooperative or local close.
func (h *HarnessTest) assertChannelClosed(hn *node.HarnessNode,
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
