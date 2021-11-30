package lntest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// WaitForChannelOpen takes a open channel client and consumes the stream to
// assert the channel is open.
func (h *HarnessTest) WaitForChannelOpen(
	client OpenChanClient) *lnrpc.ChannelPoint {

	fundingChanPoint, err := h.net.WaitForChannelOpen(h.runCtx, client)
	require.NoError(h, err, "error while waiting for channel open")

	return fundingChanPoint
}

// AssertChannelOpen asserts that a given channel outpoint is seen by the
// passed node.
func (h *HarnessTest) AssertChannelOpen(n *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) {

	err := n.WaitForNetworkChannelOpen(chanPoint)
	require.NoErrorf(h, err, "%s didn't report channel", n.Cfg.Name)
}

// AssertChannelExists asserts that an active channel identified by the
// specified channel point exists from the point-of-view of the node.
func (h *HarnessTest) AssertChannelExists(hn *HarnessNode,
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

		// Check whether our channel is active, exit early if it is.
		if channel.Active {
			return nil
		}

		return fmt.Errorf("channel point not active")
	}, DefaultTimeout)

	require.NoError(h, err, "timeout checking for channel point: %v", cp)

	return channel
}

// AssertInvoiceState asserts that a given invoice has became the desired
// state before timeout and returns the invoice found.
func (h *HarnessTest) AssertInvoiceState(hn *HarnessNode, payHash lntypes.Hash,
	state lnrpc.Invoice_InvoiceState) *lnrpc.Invoice {

	invoice, err := hn.waitForInvoiceState(payHash, state)
	require.NoError(h, err, "%s failed to wait invoice:%v to be state: %v",
		hn.Name(), payHash, state)

	return invoice
}

// AssertPaymentStatusFromStream takes a client stream and asserts the payment
// is in desired status before default timeout. The payment found is returned
// once succeeded.
func (h *HarnessTest) AssertPaymentStatusFromStream(stream PaymentClient,
	status lnrpc.Payment_PaymentStatus) *lnrpc.Payment {

	return h.AssertPaymentStatusWithTimeout(stream, status, DefaultTimeout)
}

// AssertPaymentStatusWithTimeout takes a client stream and asserts the payment
// is in desired status before the specified timeout. The payment found is
// returned once succeeded.
func (h *HarnessTest) AssertPaymentStatusWithTimeout(stream PaymentClient,
	status lnrpc.Payment_PaymentStatus,
	timeout time.Duration) *lnrpc.Payment {

	var target *lnrpc.Payment
	err := wait.NoError(func() error {
		// Consume one message. This will block until the
		// message is received.
		payment, err := stream.Recv()

		// We will not wait if there's an error returned from
		// the stream.
		if err != nil {
			return fmt.Errorf("payment stream got err: %w", err)
		}

		// Return if the desired payment state is reached.
		if payment.Status == status {
			target = payment
			return nil
		}

		// Return the err so that it can be used for debugging
		// when timeout is reached.
		return fmt.Errorf("payment status, got %v, want %v",
			payment.Status, status)
	}, timeout)

	require.NoError(h, err, "timeout while waiting payment")

	return target
}

// AssertTxInBlock asserts that a given txid can be found in the passed block.
func (h *HarnessTest) AssertTxInBlock(block *wire.MsgBlock,
	txid *chainhash.Hash) {

	blockTxes := make([]chainhash.Hash, 0)

	for _, tx := range block.Transactions {
		sha := tx.TxHash()
		blockTxes = append(blockTxes, sha)

		if bytes.Equal(txid[:], sha[:]) {
			return
		}
	}

	require.Failf(h, "tx was not included in block", "tx:%v, block has:%v",
		txid, blockTxes)
}

// AssertTxInMempool asserts a given transaction can be found in the mempool.
func (h *HarnessTest) AssertTxInMempool(op wire.OutPoint) *wire.MsgTx {
	var msgTx *wire.MsgTx

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mempool := h.GetRawMempool()

		if len(mempool) == 0 {
			return fmt.Errorf("empty mempool")
		}

		for _, txid := range mempool {
			// We require the RPC call to be succeeded and won't
			// wait for it as it's an unexpected behavior.
			tx := h.GetRawTransaction(txid)

			msgTx = tx.MsgTx()
			for _, txIn := range msgTx.TxIn {
				if txIn.PreviousOutPoint == op {
					return nil
				}
			}
		}

		return fmt.Errorf("outpoint %v not found in mempool", op)
	}, MinerMempoolTimeout)

	require.NoError(h, err, "timeout checking mempool")

	return msgTx
}

// AssertNumTxsInMempool polls until finding the desired number of transactions
// in the provided miner's mempool. It will asserrt if this number is not met
// after the given timeout.
func (h *HarnessTest) AssertNumTxsInMempool(n int) []*chainhash.Hash {
	var (
		mem []*chainhash.Hash
		err error
	)

	err = wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mem = h.GetRawMempool()
		if len(mem) == n {
			return nil
		}

		return fmt.Errorf("want %v, got %v in mempool: %v",
			n, len(mem), mem)
	}, MinerMempoolTimeout)
	require.NoError(h, err, "assert tx in mempool timeout")

	return mem
}

// AssertNodeNumChannels polls the provided node's list channels rpc until it
// reaches the desired number of total channels.
func (h *HarnessTest) AssertNodeNumChannels(hn *HarnessNode, numChannels int) {
	// Get the total number of channels.
	old := hn.state.OpenChannel.Active + hn.state.OpenChannel.Inactive

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		chanInfo := h.ListChannels(hn)

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

// AssertNumActiveHtlcs asserts that a given number of HTLCs are seen in the
// node's channels.
func (h *HarnessTest) AssertNumActiveHtlcs(hn *HarnessNode, num int) {
	old := hn.state.HTLC

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		nodeChans := h.ListChannels(hn)

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

	require.NoErrorf(h, err, "%s assert active htlcs timed out", hn.Name())
}

// AssertActiveHtlcs makes sure all the passed nodes have the _exact_ HTLCs
// matching payHashes on _all_ their channels.
func (h *HarnessTest) AssertActiveHtlcs(hn *HarnessNode, payHashes ...[]byte) {
	checkPayHashesMatch := func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		nodeChans := h.ListChannels(hn)

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
	}

	err := wait.NoError(checkPayHashesMatch, DefaultTimeout)
	require.NoError(h, err)
}

// GetNumTxsFromMempool polls until finding the desired number of transactions
// in the miner's mempool and returns the full transactions to the caller.
func (h *HarnessTest) GetNumTxsFromMempool(n int) []*wire.MsgTx {
	txids := h.AssertNumTxsInMempool(n)

	var txes []*wire.MsgTx
	for _, txid := range txids {
		tx := h.GetRawTransaction(txid)
		txes = append(txes, tx.MsgTx())
	}
	return txes
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
// tx.
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
func (h *HarnessTest) AssertChannelPendingForceClose(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) PendingForceClose {

	var target PendingForceClose

	op := h.OutPointFromChannelPoint(chanPoint)

	err := wait.NoError(func() error {
		resp := h.GetPendingChannels(hn)

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

type WaitingCloseChannel *lnrpc.PendingChannelsResponse_WaitingCloseChannel

// AssertChannelWaitingClose asserts that the given channel found in the node
// is waiting close. Returns the WaitingCloseChannel if found.
func (h *HarnessTest) AssertChannelWaitingClose(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) WaitingCloseChannel {

	var target WaitingCloseChannel

	op := h.OutPointFromChannelPoint(chanPoint)

	err := wait.NoError(func() error {
		resp := h.GetPendingChannels(hn)

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

// AssertNumPendingCloseChannels checks that a PendingChannels response from
// the node reports the expected number of pending force channels and waiting
// close channels.
func (h *HarnessTest) AssertNumPendingCloseChannels(hn *HarnessNode,
	expWaitingClose, expPendingForceClose int) {

	oldWaiting := hn.state.CloseChannel.WaitingClose
	oldForce := hn.state.CloseChannel.PendingForceClose

	err := wait.NoError(func() error {
		resp := h.GetPendingChannels(hn)
		total := len(resp.WaitingCloseChannels)

		got := total - oldWaiting
		if got != expWaitingClose {
			return errNumNotMatched(hn.Name(),
				"waiting close channels", expWaitingClose, got,
				total, oldWaiting)
		}

		total = len(resp.PendingForceClosingChannels)
		got = total - oldForce
		if got != expPendingForceClose {
			return errNumNotMatched(hn.Name(),
				"pending force close channels",
				expPendingForceClose, got, total, oldForce)
		}

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "assert pending channels timeout")
}

// AssertChannelPolicyUpdate checks that the required policy update has
// happened on the given node.
func (h *HarnessTest) AssertChannelPolicyUpdate(node *HarnessNode,
	advertisingNode *HarnessNode, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint, includeUnannounced bool) {

	require.NoError(
		h, node.WaitForChannelPolicyUpdate(
			advertisingNode, policy,
			chanPoint, includeUnannounced,
		), "%s: error while waiting for channel update", node.Name(),
	)
}

// WaitForChannelClose will consume a close channel stream and asserts the
// channel is closed, return the closing txid if succeeded.
func (h *HarnessTest) WaitForChannelClose(
	closeUpdates CloseChanClient) *chainhash.Hash {

	var txid *chainhash.Hash

	err := wait.NoError(func() error {
		closingTxid, err := h.net.WaitForChannelClose(closeUpdates)
		txid = closingTxid
		return err
	}, ChannelCloseTimeout)
	require.NoError(h, err, "error while waiting for channel close")

	return txid
}

// assertPeerConnected asserts that the given node b is connected to a.
func (h *HarnessTest) assertPeerConnected(a, b *HarnessNode) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		resp := h.ListPeers(a)

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

// AssertConnected asserts that two peers are connected.
func (h *HarnessTest) AssertConnected(a, b *HarnessNode) {
	h.assertPeerConnected(a, b)
	h.assertPeerConnected(b, a)
}

// assertPeerNotConnected asserts that the given node b is not connected to a.
func (h *HarnessTest) assertPeerNotConnected(a, b *HarnessNode) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		resp := h.ListPeers(a)

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
func (h *HarnessTest) AssertNotConnected(a, b *HarnessNode) {
	h.assertPeerNotConnected(a, b)
	h.assertPeerNotConnected(b, a)
}

// AssertPaymentStatus asserts that the given node list a payment with the
// given preimage has the expected status. It also checks that the payment has
// the expected preimage, which is empty when it's not settled and matches the
// given preimage when it's succeeded.
func (h *HarnessTest) AssertPaymentStatus(hn *HarnessNode,
	preimage lntypes.Preimage,
	status lnrpc.Payment_PaymentStatus) *lnrpc.Payment {

	var target *lnrpc.Payment

	err := wait.NoError(func() error {
		p := h.findPayment(hn, preimage)
		if status == p.Status {
			target = p
			return nil
		}
		return fmt.Errorf("payment: %v status not match, want %s "+
			"got %s", preimage, status, p.Status)
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

// AssertHTLCStage asserts the HTLC is in the expected stage or timeout.
func (h *HarnessTest) AssertHTLCStage(hn *HarnessNode, stage uint32) {
	checkStage := func() error {
		pendingChanResp := h.GetPendingChannels(hn)
		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			return fmt.Errorf("zero pending force closing channels")
		}

		ch := pendingChanResp.PendingForceClosingChannels[0]
		if ch.LimboBalance == 0 {
			return fmt.Errorf("zero balance")
		}

		if len(ch.PendingHtlcs) != 1 {
			return fmt.Errorf("got %d pending htlcs, want 1",
				len(ch.PendingHtlcs))
		}
		if stage != ch.PendingHtlcs[0].Stage {
			return fmt.Errorf("got stage: %v, want stage: %v",
				ch.PendingHtlcs[0].Stage, stage)
		}

		return nil
	}

	require.NoErrorf(h, wait.NoError(checkStage, DefaultTimeout),
		"timeout waiting for htlc stage")
}

// AssertChannelBalanceResp makes a ChannelBalance request and checks the
// returned response matches the expected.
func (h *HarnessTest) AssertChannelBalanceResp(hn *HarnessNode,
	expected *lnrpc.ChannelBalanceResponse) { // nolint:interfacer

	resp := h.GetChannelBalance(hn)
	require.True(h, proto.Equal(expected, resp), "balance is incorrect "+
		"got: %v, want: %v", resp, expected,
	)
}

// GetChannelCommitType retrieves the active channel commitment type for the
// given chan point.
func (h *HarnessTest) GetChannelCommitType(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) lnrpc.CommitmentType {

	channels := h.ListChannels(hn)
	for _, c := range channels.Channels {
		if c.ChannelPoint == txStr(chanPoint) {
			return c.CommitmentType
		}
	}

	require.Fail(h, "get channel commit type failed", "channel point %v "+
		"not found", chanPoint)

	return 0
}

// AssertNumOpenChannelsPending asserts that a pair of nodes have the expected
// number of pending channels between them.
func (h *HarnessTest) AssertNumOpenChannelsPending(alice, bob *HarnessNode,
	expected int) {

	oldAlice := alice.state.OpenChannel.Pending
	oldBob := bob.state.OpenChannel.Pending

	err := wait.NoError(func() error {
		aliceChans := h.GetPendingChannels(alice)
		aliceTotal := len(aliceChans.PendingOpenChannels)
		bobChans := h.GetPendingChannels(bob)
		bobTotal := len(bobChans.PendingOpenChannels)

		aliceNumChans := aliceTotal - oldAlice
		bobNumChans := bobTotal - oldBob

		if aliceNumChans != expected {
			return errNumNotMatched(alice.Name(),
				"pending open channels", expected,
				aliceNumChans, aliceTotal, oldAlice)
		}

		if bobNumChans != expected {
			return errNumNotMatched(bob.Name(),
				"pending open channels", expected,
				bobNumChans, bobTotal, oldBob)
		}

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "num of pending open channels not match")
}

// WaitForChannelPendingForceClose waits for the node to report that the
// channel is pending force close, and that the UTXO nursery is aware of it.
func (h *HarnessTest) WaitForChannelPendingForceClose(hn *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint) {

	txid, err := lnrpc.GetChanPointFundingTxid(fundingChanPoint)
	require.NoError(h, err, "failed to get chan point from txid")

	op := wire.OutPoint{
		Hash:  *txid,
		Index: fundingChanPoint.OutputIndex,
	}

	err = wait.NoError(func() error {
		pendingChanResp := h.GetPendingChannels(hn)

		forceClose, err := FindForceClosedChannel(pendingChanResp, &op)
		if err != nil {
			return err
		}

		// We must wait until the UTXO nursery has received the channel
		// and is aware of its maturity height.
		if forceClose.MaturityHeight == 0 {
			return fmt.Errorf("channel had maturity height of 0")
		}

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while finding force closed channels")
}

// AssertAmountPaid checks that the ListChannels command of the provided
// node list the total amount sent and received as expected for the
// provided channel.
func (h *HarnessTest) AssertAmountPaid(channelName string, hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint, amountSent, amountReceived int64) {

	checkAmountPaid := func() error {
		// Find the targeted channel.
		channel, err := h.findChannel(hn, chanPoint)
		if err != nil {
			return fmt.Errorf("assert amount failed: %v", err)
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

// WaitForNodeBlockHeight queries the node for its current block height until
// it reaches the passed height.
func (h *HarnessTest) WaitForNodeBlockHeight(hn *HarnessNode, height int32) {
	err := wait.NoError(func() error {
		info := h.GetInfo(hn)
		if int32(info.BlockHeight) != height {
			return fmt.Errorf("expected block height to "+
				"be %v, was %v", height, info.BlockHeight)
		}
		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while waiting for height")
}

// AssertNumEdges checks that an expected number of edges can be found in the
// node specified.
func (h *HarnessTest) AssertNumEdges(hn *HarnessNode,
	expected int, includeUnannounced bool) []*lnrpc.ChannelEdge {

	var edges []*lnrpc.ChannelEdge

	old := hn.state.Edge.Public
	if includeUnannounced {
		old = hn.state.Edge.Total
	}

	err := wait.NoError(func() error {
		chanGraph := h.DescribeGraph(hn, includeUnannounced)
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

// AssertLastHTLCError checks that the last sent HTLC of the last payment sent
// by the given node failed with the expected failure code.
func (h *HarnessTest) AssertLastHTLCError(hn *HarnessNode,
	code lnrpc.Failure_FailureCode) {

	// Use -1 to specify the last HTLC.
	h.assertHTLCError(hn, code, -1)
}

// AssertFirstHTLCError checks that the first HTLC of the last payment sent
// by the given node failed with the expected failure code.
func (h *HarnessTest) AssertFirstHTLCError(hn *HarnessNode,
	code lnrpc.Failure_FailureCode) {

	// Use 0 to specify the first HTLC.
	h.assertHTLCError(hn, code, 0)
}

// assertLastHTLCError checks that the HTLC at the specified index of the last
// payment sent by the given node failed with the expected failure code.
func (h *HarnessTest) assertHTLCError(hn *HarnessNode,
	code lnrpc.Failure_FailureCode, index int) {

	err := wait.NoError(func() error {
		paymentsResp := h.ListPayments(hn, true)

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

// AssertTxAtHeight gets all of the transactions that a node's wallet has a
// record of at the target height, and finds and returns the tx with the target
// txid, failing if it is not found.
func (h *HarnessTest) AssertTxAtHeight(hn *HarnessNode, height int32,
	txid string) *lnrpc.Transaction {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	txns, err := hn.rpc.LN.GetTransactions(
		ctxt, &lnrpc.GetTransactionsRequest{
			StartHeight: height,
			EndHeight:   height,
		},
	)
	require.NoError(h, err, "could not get transactions")

	for _, tx := range txns.Transactions {
		if tx.TxHash == txid {
			return tx
		}
	}

	require.Failf(h, "fail to find tx", "tx:%v not found at height:%v",
		txid, height)

	return nil
}

// AssertChannelPolicy asserts that the passed node's known channel policy for
// the passed chanPoint is consistent with the expected policy values.
func (h *HarnessTest) AssertChannelPolicy(hn *HarnessNode,
	advertisingNode string, expectedPolicy *lnrpc.RoutingPolicy,
	chanPoints ...*lnrpc.ChannelPoint) {

	policies := h.getChannelPolicies(hn, advertisingNode, chanPoints...)
	for _, policy := range policies {
		err := CheckChannelPolicy(policy, expectedPolicy)
		require.NoError(h, err, "check policy failed")
	}
}

// QueryChannelByChanPoint tries to find a channel matching the channel point
// and asserts. It returns the channel found.
func (h *HarnessTest) QueryChannelByChanPoint(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.Channel {

	channel, err := h.findChannel(hn, chanPoint)
	require.NoError(h, err, "failed to query channel")
	return channel
}

// AssertLocalBalance checks the local balance of the given channel is
// expected. The channel found using the specified channel point is returned.
func (h *HarnessTest) AssertLocalBalance(hn *HarnessNode,
	cp *lnrpc.ChannelPoint, balance int64) *lnrpc.Channel {

	var result *lnrpc.Channel

	// Get the funding point.
	err := wait.NoError(func() error {
		// Find the target channel first.
		target, err := h.findChannel(hn, cp)

		// Exit early if the channel is not found.
		if err != nil {
			return fmt.Errorf("check balance failed: %v", err)
		}

		result = target

		// Check local balance.
		if target.LocalBalance == balance {
			return nil
		}

		return fmt.Errorf("balance is incorrect, got %v, expected %v",
			target.LocalBalance, balance)
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while chekcing for balance")

	return result
}

// AssertChannelState asserts the channel state by checking the values in
// fields, LocalBalance, RemoteBalance and num of PendingHtlcs.
func (h *HarnessTest) AssertChannelState(hn *HarnessNode,
	cp *lnrpc.ChannelPoint, localBalance,
	remoteBalance int64, numPendingHtlcs int) *lnrpc.Channel {

	var result *lnrpc.Channel

	// Get the funding point.
	err := wait.NoError(func() error {
		// Find the target channel first.
		target, err := h.findChannel(hn, cp)

		// Exit early if the channel is not found.
		if err != nil {
			return fmt.Errorf("check balance failed: %v", err)
		}

		if len(target.PendingHtlcs) != numPendingHtlcs {
			return fmt.Errorf("pending htlcs is "+
				"incorrect, got %v, expected %v",
				len(target.PendingHtlcs), 0)
		}

		if target.LocalBalance != localBalance {
			return fmt.Errorf("local balance is "+
				"incorrect, got %v, expected %v",
				target.LocalBalance, localBalance)
		}

		if target.RemoteBalance != remoteBalance {
			return fmt.Errorf("remote balance is "+
				"incorrect, got %v, expected %v",
				target.RemoteBalance, remoteBalance)
		}

		result = target
		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while chekcing for balance")

	return result
}

// AssertNumUpdates checks the num of updates is expected from the given
// channel.
//
// NOTE: doesn't wait.
func (h *HarnessTest) AssertNumUpdates(hn *HarnessNode,
	num uint64, cp *lnrpc.ChannelPoint) {

	old := int(hn.state.OpenChannel.NumUpdates)

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
	require.NoError(h, err, "timeout while chekcing for num of updates")
}

// AssertNodeStarted waits until the node is fully started or asserts a timeout
// error.
func (h *HarnessTest) AssertNodeStarted(hn *HarnessNode) {
	require.NoError(h, hn.WaitUntilServerActive())
}

// GetUTOXs gets the number of newly created UTOXs within the current test
// scope.
func (h *HarnessTest) GetUTXOs(hn *HarnessNode, account string,
	max, min int32) []*lnrpc.Utxo {

	indexOffset := hn.state.UTXO.Confirmed
	if max == 0 {
		indexOffset = hn.state.UTXO.Unconfirmed
	}

	resp := h.ListUnspent(hn, account, max, min)
	require.GreaterOrEqual(h, len(resp.Utxos), indexOffset,
		"wrong utxo state")

	// We assume the utxos arrive in time-wise ascending order, as the
	// oldest utxos comes at the front of the slice.
	return resp.Utxos[indexOffset:]
}

// AssertNumUTXOs waits for the given number of UTXOs to be available or fails
// if that isn't the case before the default timeout.
func (h *HarnessTest) AssertNumUTXOs(hn *HarnessNode, expectedUtxos int,
	max, min int32) []*lnrpc.Utxo {

	old := hn.state.UTXO.Confirmed
	if max == 0 {
		old = hn.state.UTXO.Unconfirmed
	}

	var utxos []*lnrpc.Utxo
	err := wait.NoError(func() error {
		resp := h.ListUnspent(hn, "", max, min)
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

// AssertZombieChannel asserts that a given channel found using the chanID is
// marked as zombie.
func (h *HarnessTest) AssertZombieChannel(hn *HarnessNode, chanID uint64) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	err := wait.NoError(func() error {
		_, err := hn.rpc.LN.GetChanInfo(
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

// AssertNumChannelUpdates asserts that a given number of channel updates has
// been seen in the specified node.
func (h *HarnessTest) AssertNumChannelUpdates(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint, num int) {

	op := h.OutPointFromChannelPoint(chanPoint)
	err := hn.WaitForNetworkNumChanUpdates(op, num)
	require.NoError(h, err, "failed to assert num of channel updates")
}

// AssertNumNodeAnns asserts that a given number of node announcements has been
// seen in the specified node.
func (h *HarnessTest) AssertNumNodeAnns(hn *HarnessNode,
	pubkey string, num int) {

	// We will get the current number of channel updates first and add it
	// to our expected number of newly created channel updates.
	err := hn.WaitForNetworkNumNodeUpdates(pubkey, num)
	require.NoError(h, err, "failed to assert num of channel updates")
}

// AssertNumPolicyUpdates asserts that a given number of channel policy updates
// has been seen in the specified node.
func (h *HarnessTest) AssertNumPolicyUpdates(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint, advertisingNode *HarnessNode, num int) {

	op := h.OutPointFromChannelPoint(chanPoint)

	var policies []*PolicyUpdate

	err := wait.NoError(func() error {
		policyMap := hn.GetPolicyUpdates(op)
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

// AssertChannelClosedFromGraph asserts a given channel is closed by checking
// the graph topology subscription of the specified node. Returns the closed
// channel update if found.
func (h *HarnessTest) AssertChannelClosedFromGraph(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.ClosedChannelUpdate {

	closedChan, err := hn.WaitForNetworkChannelClose(chanPoint)
	require.NoError(h, err, "failed to wait for channel close")

	return closedChan
}

// AssertHtlcEvents consumes events from a client and ensures that they are of
// the expected type and contain the expected number of forwards, forward
// failures and settles.
func (h *HarnessTest) AssertHtlcEvents(client HtlcEventsClient,
	fwdCount, fwdFailCount, settleCount, linkFailCount int,
	userType routerrpc.HtlcEvent_EventType) []*routerrpc.HtlcEvent {

	var forwards, forwardFails, settles, linkFails int

	numEvents := fwdCount + fwdFailCount + settleCount + linkFailCount
	events := make([]*routerrpc.HtlcEvent, 0)

	for i := 0; i < numEvents; i++ {
		event := h.ReceiveHtlcEvent(client)
		require.Equalf(h, userType, event.EventType,
			"wrong event type, want %v got %v",
			userType, event.EventType)

		events = append(events, event)

		switch event.Event.(type) {
		case *routerrpc.HtlcEvent_ForwardEvent:
			forwards++

		case *routerrpc.HtlcEvent_ForwardFailEvent:
			forwardFails++

		case *routerrpc.HtlcEvent_SettleEvent:
			settles++

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

// AssertFeeReport checks that the fee report from the given node has the
// desired day, week, and month sum values.
func (h *HarnessTest) AssertFeeReport(hn *HarnessNode,
	day, week, month int) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	feeReport, err := hn.rpc.LN.FeeReport(ctxt, &lnrpc.FeeReportRequest{})
	require.NoError(h, err, "unable to query for fee report")

	require.EqualValues(h, day, feeReport.DayFeeSum, "day fee mismatch")
	require.EqualValues(h, week, feeReport.DayFeeSum, "day week mismatch")
	require.EqualValues(h, month, feeReport.DayFeeSum, "day month mismatch")
}

// WaitForBlockchainSync waits until the node is synced to chain.
func (h *HarnessTest) WaitForBlockchainSync(hn *HarnessNode) {
	err := hn.WaitForBlockchainSync()
	require.NoError(h, err, "timeout waiting for blockchain sync")
}

// AssertTransactionInWallet asserts a given txid can be found in the node's
// wallet.
func (h *HarnessTest) AssertTransactionInWallet(hn *HarnessNode,
	txid chainhash.Hash) {

	err := wait.NoError(func() error {
		txResp := h.GetTransactions(hn)
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
func (h *HarnessTest) AssertTransactionNotInWallet(hn *HarnessNode,
	txid chainhash.Hash) {

	err := wait.NoError(func() error {
		txResp := h.GetTransactions(hn)
		for _, txn := range txResp.Transactions {
			if txn.TxHash == txid.String() {
				return fmt.Errorf("expected txid=%v to be "+
					"not found", txid)
			}
		}

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "failed to assert tx not found")
}

// GetOutputIndex returns the output index of the given address in the given
// transaction.
func (h *HarnessTest) GetOutputIndex(txid *chainhash.Hash, addr string) int {
	h.Helper()

	// We'll then extract the raw transaction from the mempool in order to
	// determine the index of the p2tr output.
	tx := h.GetRawTransaction(txid)

	p2trOutputIndex := -1
	for i, txOut := range tx.MsgTx().TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			txOut.PkScript, h.Miner().ActiveNet,
		)
		require.NoError(h, err)

		if addrs[0].String() == addr {
			p2trOutputIndex = i
		}
	}
	require.Greater(h, p2trOutputIndex, -1)

	return p2trOutputIndex
}

// errNumNotMatched is a helper method to return a nicely formatted error.
func errNumNotMatched(name string, subject string,
	want, got, total, old int) error {

	return fmt.Errorf("%s: assert %s failed: want %d, got: %d, total: "+
		"%d, previously had: %d", name, subject, want, got, total, old)
}

// AssertNumPayments asserts that the number of payments made within the test
// scope is as expected.
func (h *HarnessTest) AssertNumPayments(hn *HarnessNode, num int,
	includeIncomplete bool) []*lnrpc.Payment {

	have := hn.state.Payment.Completed
	if includeIncomplete {
		have = hn.state.Payment.Total
	}

	var payments []*lnrpc.Payment
	err := wait.NoError(func() error {
		resp := h.ListPayments(hn, includeIncomplete)

		payments = resp.Payments
		if len(payments) == num {
			return nil
		}

		return errNumNotMatched(hn.Name(), "num of payments",
			num, len(payments), have+len(payments), have)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking num of payments")

	return payments
}

// AssertNumInvoices asserts that the number of invoices made within the test
// scope is as expected.
func (h *HarnessTest) AssertNumInvoices(hn *HarnessNode,
	num int) []*lnrpc.Invoice {

	have := hn.state.Invoice.Total

	var invoices []*lnrpc.Invoice
	err := wait.NoError(func() error {
		resp := h.ListInvoices(hn)

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

// GetChannelBalance gets the channel balances which are changed within the
// current test.
func (h *HarnessTest) GetChannelBalance(
	hn *HarnessNode) *lnrpc.ChannelBalanceResponse {

	resp := h.ChannelBalance(hn)

	calculate := func(a, b *lnrpc.Amount) *lnrpc.Amount {
		return &lnrpc.Amount{
			Sat:  a.GetSat() - b.GetSat(),
			Msat: a.GetMsat() - b.GetMsat(),
		}
	}
	local := calculate(resp.LocalBalance, hn.state.Balance.LocalBalance)
	remote := calculate(resp.RemoteBalance, hn.state.Balance.RemoteBalance)
	unLocal := calculate(
		resp.UnsettledLocalBalance,
		hn.state.Balance.UnsettledLocalBalance,
	)
	unRemote := calculate(
		resp.UnsettledRemoteBalance,
		hn.state.Balance.UnsettledRemoteBalance,
	)
	pendingLocal := calculate(
		resp.PendingOpenLocalBalance,
		hn.state.Balance.PendingOpenLocalBalance,
	)
	pendingRemote := calculate(
		resp.PendingOpenRemoteBalance,
		hn.state.Balance.PendingOpenRemoteBalance,
	)

	balance := resp.Balance - hn.state.Balance.Balance // nolint:staticcheck

	pendingBalance := resp.PendingOpenBalance - // nolint:staticcheck
		hn.state.Balance.PendingOpenBalance

	return &lnrpc.ChannelBalanceResponse{
		LocalBalance:             local,
		RemoteBalance:            remote,
		UnsettledLocalBalance:    unLocal,
		UnsettledRemoteBalance:   unRemote,
		PendingOpenLocalBalance:  pendingLocal,
		PendingOpenRemoteBalance: pendingRemote,

		// Deprecated fields.
		Balance:            balance,        // nolint:staticcheck
		PendingOpenBalance: pendingBalance, // nolint:staticcheck

	}
}

// GetWalletBalance makes a RPC call to WalletBalance which are changed within
// the current test.
func (h *HarnessTest) GetWalletBalance(
	hn *HarnessNode) *lnrpc.WalletBalanceResponse {

	resp := h.WalletBalance(hn)

	total := resp.TotalBalance - hn.state.Wallet.TotalBalance
	confirmed := resp.ConfirmedBalance - hn.state.Wallet.ConfirmedBalance
	unconfirmed := resp.UnconfirmedBalance -
		hn.state.Wallet.UnconfirmedBalance

	accounts := make(map[string]*lnrpc.WalletAccountBalance)
	for name, balance := range resp.AccountBalance {
		account := &lnrpc.WalletAccountBalance{
			ConfirmedBalance:   balance.ConfirmedBalance,
			UnconfirmedBalance: balance.UnconfirmedBalance,
		}
		old, ok := hn.state.Wallet.AccountBalance[name]
		if ok {
			account.ConfirmedBalance -= old.ConfirmedBalance
			account.UnconfirmedBalance -= old.UnconfirmedBalance
		}

		accounts[name] = account
	}

	return &lnrpc.WalletBalanceResponse{
		TotalBalance:       total,
		ConfirmedBalance:   confirmed,
		UnconfirmedBalance: unconfirmed,
		AccountBalance:     accounts,
	}
}
