package itest

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupThreeHopNetwork creates a network with the following topology and
// liquidity:
// Alice (100k)----- Bob (100k) ----- Carol (100k) ----- Dave
//
// The funding outpoint for AB / BC / CD are returned in-order.
func setupThreeHopNetwork(t *harnessTest, net *lntest.NetworkHarness,
	carol, dave *lntest.HarnessNode) []lnwire.ShortChannelID {

	const chanAmt = btcutil.Amount(100000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPointAlice := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChan, err := getChanInfo(net.Alice)
	require.NoError(t.t, err, "alice channel")

	// Create a channel between bob and carol.
	t.lndHarness.EnsureConnected(t.t, net.Bob, carol)
	chanPointBob := openChannelAndAssert(
		t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointBob)

	// Our helper function expects one channel, so we lookup using carol.
	bobChan, err := getChanInfo(carol)
	require.NoError(t.t, err, "bob channel")

	// Fund carol and connect her and dave so that she can create a channel
	// between them.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)
	net.ConnectNodes(t.t, carol, dave)

	t.lndHarness.EnsureConnected(t.t, carol, dave)
	chanPointCarol := openChannelAndAssert(
		t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	// As above, we use the helper that only expects one channel so we
	// lookup on dave's end.
	carolChan, err := getChanInfo(dave)
	require.NoError(t.t, err, "carol chan")

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
			require.NoError(t.t, err, "unable to get txid")

			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			err = node.WaitForNetworkChannelOpen(chanPoint)
			require.NoError(t.t, err, "%s(%d): timeout waiting "+
				"for channel(%s) open", nodeNames[i],
				node.NodeID, point)
		}
	}

	// Update our channel policy so that our upfront fee will hit 1 sat (1%
	// of our base fee). We do this so that the change in balance will show
	// up on our ListChannels RPC, which is expressed in satoshis.
	//
	// We only need to set outgoing edges and ensure Alice sees them
	// because she's the one who will be routing, and fees are charged on
	// the outgoing edge.
	setChannelPolicy(t, chanPointBob, net.Bob, net.Alice)
	setChannelPolicy(t, chanPointCarol, carol, net.Alice)

	return []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(aliceChan.ChanId),
		lnwire.NewShortChanIDFromInt(bobChan.ChanId),
		lnwire.NewShortChanIDFromInt(carolChan.ChanId),
	}
}

// setChanelPolicy updates a channel's policy and asserts that a watching node
// sees the update in its gossip. We set the base fee on our policy such that
// a 1% upfront fee will be 1 satoshi, and reflect in our channel balances.
//
//nolint:gomnd
func setChannelPolicy(t *harnessTest, channel *lnrpc.ChannelPoint,
	updatingNode, watchingNode *lntest.HarnessNode) {

	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Lookup our existing policy and copying over some existing values (so
	// that we have a valid update), setting only our base fee.
	policies := getChannelPolicies(
		t, updatingNode, updatingNode.PubKeyStr, channel,
	)
	require.Len(t.t, policies, 1)

	req := &lnrpc.PolicyUpdateRequest{
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: channel,
		},
		BaseFeeMsat:   100_000,
		TimeLockDelta: policies[0].TimeLockDelta,
	}

	resp, err := updatingNode.UpdateChannelPolicy(ctxt, req)
	require.NoError(t.t, err)
	require.Len(t.t, resp.FailedUpdates, 0)

	pred := func() bool {
		policies := getChannelPolicies(
			t, watchingNode, updatingNode.PubKeyStr, channel,
		)
		if len(policies) != 1 {
			return false
		}

		return policies[0].FeeBaseMsat == req.BaseFeeMsat
	}

	require.NoError(t.t, wait.Predicate(pred, defaultTimeout))
}

func lookupChannel(ctxb context.Context, t *testing.T, node *lntest.HarnessNode,
	channelID uint64) *lnrpc.Channel {

	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := node.ListChannels(ctxt, &lnrpc.ListChannelsRequest{})
	require.NoError(t, err)
	cancel()

	for _, channel := range resp.Channels {
		if channel.ChanId != channelID {
			continue
		}

		return channel
	}

	t.Fatalf("node: %v does not have channel: %v", node.PubKeyStr,
		channelID)

	return nil
}

//nolint:gomnd
func testUpfrontFees(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	carol := net.NewNode(t.t, "Carol", nil)
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "Dave", nil)
	defer shutdownAndAssert(net, t, dave)

	// Create a hodl invoice so that we can hold the pending htlc on the
	// receiver while we assert that upfront fees have been pushed along
	// the route.
	preimage := [32]byte{1, 2, 3}
	hash := sha256.Sum256(preimage[:])
	req := &invoicesrpc.AddHoldInvoiceRequest{
		ValueMsat: 10_000,
		Hash:      hash[:],
	}

	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := dave.InvoicesClient.AddHoldInvoice(ctxt, req)
	require.NoError(t.t, err)
	cancel()

	// Setup Alice --- Bob --- Carol --- Dave channels.
	channels := setupThreeHopNetwork(t, net, carol, dave)

	// Get starting balances for each channel.
	aliceBob0 := lookupChannel(ctxb, t.t, net.Alice, channels[0].ToUint64())
	bobCarol0 := lookupChannel(ctxb, t.t, net.Bob, channels[1].ToUint64())
	carolDave0 := lookupChannel(ctxb, t.t, carol, channels[2].ToUint64())

	ctxt, cancel = context.WithCancel(ctxb)
	defer cancel()
	sendResp, err := net.Alice.RouterClient.SendPaymentV2(
		ctxt, &routerrpc.SendPaymentRequest{
			NoInflightUpdates: true,
			PaymentRequest:    resp.PaymentRequest,
			TimeoutSeconds:    120,
			FeeLimitMsat:      1000000,
		},
	)
	require.NoError(t.t, err)

	// Assert that the HTLC is locked in along our route.
	nodes := []*lntest.HarnessNode{
		net.Alice, net.Bob, carol, dave,
	}

	pred := func() bool {
		return assertActiveHtlcs(nodes, hash[:]) == nil
	}
	require.NoError(t.t, wait.Predicate(pred, defaultTimeout))

	// Wait for the invoice to be updated to an accepted state (we need
	// to exchange some commitments) before we settle. We also know that
	// the HTLC is fully locked in once the HTLC is accepted, so we can
	// check balances.
	pred = func() bool {
		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()

		req := &invoicesrpc.LookupInvoiceMsg{
			InvoiceRef: &invoicesrpc.LookupInvoiceMsg_PaymentHash{
				PaymentHash: hash[:],
			},
		}

		inv, err := dave.InvoicesClient.LookupInvoiceV2(ctxt, req)
		if err != nil {
			return false
		}

		return inv.State == lnrpc.Invoice_ACCEPTED
	}
	require.NoError(t.t, wait.Predicate(pred, defaultTimeout))

	// Check that nodes have pushed the upfront fee along the route. The
	// HTLC is pending here so we can use
	aliceBob1 := lookupChannel(ctxb, t.t, net.Alice, channels[0].ToUint64())
	bobCarol1 := lookupChannel(ctxb, t.t, net.Bob, channels[1].ToUint64())
	carolDave1 := lookupChannel(ctxb, t.t, carol, channels[2].ToUint64())

	// Alice --- Bob: assert that Alice has less funds than before, and
	// Bob has more.
	require.Less(t.t, aliceBob1.LocalBalance, aliceBob0.LocalBalance)
	require.Greater(t.t, aliceBob1.RemoteBalance, aliceBob0.RemoteBalance)

	// Bob --- Carol: assert that Bob has less funds than before, and Bob
	// has more.
	require.Less(t.t, bobCarol1.LocalBalance, bobCarol0.LocalBalance)
	require.Greater(t.t, bobCarol1.RemoteBalance, bobCarol0.RemoteBalance)

	// Carol --- Dave: assert that Carol has less funds than before, and
	// Dave has more.
	require.Less(t.t, carolDave1.LocalBalance, carolDave0.LocalBalance)
	require.Greater(t.t, carolDave1.RemoteBalance, carolDave0.RemoteBalance)

	// Finally, settle the HTLC and assert that the payment is a success.
	settleReq := &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage[:],
	}

	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	_, err = dave.InvoicesClient.SettleInvoice(ctxt, settleReq)
	cancel()
	require.NoError(t.t, err)

	pmt, err := sendResp.Recv()
	require.NoError(t.t, err)

	assert.Equal(t.t, lnrpc.Payment_SUCCEEDED, pmt.Status)
	assert.Equal(t.t, lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
		pmt.FailureReason)
}
