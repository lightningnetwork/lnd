package itest

import (
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// testSendToRouteMultiPath tests that we are able to successfully route a
// payment using multiple shards across different paths, by using SendToRoute.
func testSendToRouteMultiPath(ht *lntest.HarnessTest) {
	ctx := newMppTestContext(ht)

	// To ensure the payment goes through separate paths, we'll set a
	// channel size that can only carry one shard at a time. We'll divide
	// the payment into 3 shards.
	const (
		paymentAmt = btcutil.Amount(300000)
		shardAmt   = paymentAmt / 3
		chanAmt    = shardAmt * 3 / 2
	)

	// Set up a network with three different paths Alice <-> Bob.
	//              _ Eve _
	//             /       \
	// Alice -- Carol ---- Bob
	//      \              /
	//       \__ Dave ____/
	//
	ctx.openChannel(ctx.carol, ctx.bob, chanAmt)
	ctx.openChannel(ctx.dave, ctx.bob, chanAmt)
	ctx.openChannel(ctx.alice, ctx.dave, chanAmt)
	ctx.openChannel(ctx.eve, ctx.bob, chanAmt)
	ctx.openChannel(ctx.carol, ctx.eve, chanAmt)

	// Since the channel Alice-> Carol will have to carry two
	// shards, we make it larger.
	ctx.openChannel(ctx.alice, ctx.carol, chanAmt+shardAmt)

	defer ctx.closeChannels()

	// Make Bob create an invoice for Alice to pay.
	payReqs, rHashes, invoices := ht.CreatePayReqs(ctx.bob, paymentAmt, 1)

	rHash := rHashes[0]
	payReq := payReqs[0]

	decodeResp := ht.DecodePayReq(ctx.bob, payReq)
	payAddr := decodeResp.PaymentAddr

	// Subscribe the invoice.
	stream := ht.SubscribeSingleInvoice(ctx.bob, rHash)

	// We'll send shards along three routes from Alice.
	sendRoutes := [][]*lntest.HarnessNode{
		{ctx.carol, ctx.bob},
		{ctx.dave, ctx.bob},
		{ctx.carol, ctx.eve, ctx.bob},
	}

	responses := make(chan *lnrpc.HTLCAttempt, len(sendRoutes))
	for _, hops := range sendRoutes {
		// Build a route for the specified hops.
		r := ctx.buildRoute(shardAmt, ctx.alice, hops)

		// Set the MPP records to indicate this is a payment shard.
		hop := r.Hops[len(r.Hops)-1]
		hop.TlvPayload = true
		hop.MppRecord = &lnrpc.MPPRecord{
			PaymentAddr:  payAddr,
			TotalAmtMsat: int64(paymentAmt * 1000),
		}

		// Send the shard.
		sendReq := &routerrpc.SendToRouteRequest{
			PaymentHash: rHash,
			Route:       r,
		}

		// We'll send all shards in their own goroutine, since
		// SendToRoute will block as long as the payment is in flight.
		go func() {
			resp := ht.SendToRouteV2(ctx.alice, sendReq)
			responses <- resp
		}()
	}

	// Wait for all responses to be back, and check that they all
	// succeeded.
	timer := time.After(defaultTimeout)
	for range sendRoutes {
		var resp *lnrpc.HTLCAttempt
		select {
		case resp = <-responses:
		case <-timer:
			require.Fail(ht, "response not received")
		}

		require.Nil(ht, resp.Failure, "received payment failure")

		// All shards should come back with the preimage.
		require.Equal(ht, resp.Preimage, invoices[0].RPreimage,
			"preimage doesn't match")
	}

	// assertNumHtlcs is a helper that checks the node's latest payment,
	// and asserts it was split into num shards.
	assertNumHtlcs := func(node *lntest.HarnessNode, num int) {
		var preimage lntypes.Preimage
		copy(preimage[:], invoices[0].RPreimage)

		payment := ht.AssertPaymentStatus(
			node, preimage, lnrpc.Payment_SUCCEEDED,
		)

		htlcs := payment.Htlcs
		require.NotEmpty(ht, htlcs, "no htlcs")

		succeeded := 0
		for _, htlc := range htlcs {
			if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
				succeeded++
			}
		}
		require.Equal(ht, num, succeeded, "HTLCs not matched")
	}

	// assertSettledInvoice checks that the invoice for the given payment
	// hash is settled, and has been paid using num HTLCs.
	assertSettledInvoice := func(rhash []byte, num int) {
		var payHash lntypes.Hash
		copy(payHash[:], rhash)
		inv := ht.AssertInvoiceState(stream, lnrpc.Invoice_SETTLED)

		// Assert that the amount paid to the invoice is correct.
		require.EqualValues(ht, paymentAmt, inv.AmtPaidSat,
			"incorrect payment amt")

		require.Len(ht, inv.Htlcs, num, "wrong num of HTLCs")
	}

	// Finally check that the payment shows up with three settled HTLCs in
	// Alice's list of payments...
	assertNumHtlcs(ctx.alice, 3)

	// ...and in Bob's list of paid invoices.
	assertSettledInvoice(rHash, 3)
}

// testSendMultiPathPayment tests that we are able to successfully route a
// payment using multiple shards across different paths.
func testSendMultiPathPayment(ht *lntest.HarnessTest) {
	ctx := newMppTestContext(ht)

	const paymentAmt = btcutil.Amount(300000)

	// Set up a network with three different paths Alice <-> Bob. Channel
	// capacities are set such that the payment can only succeed if (at
	// least) three paths are used.
	//
	//              _ Eve _
	//             /       \
	// Alice -- Carol ---- Bob
	//      \              /
	//       \__ Dave ____/
	//
	ctx.openChannel(ctx.carol, ctx.bob, 135000)
	ctx.openChannel(ctx.alice, ctx.carol, 235000)
	ctx.openChannel(ctx.dave, ctx.bob, 135000)
	ctx.openChannel(ctx.alice, ctx.dave, 135000)
	ctx.openChannel(ctx.eve, ctx.bob, 135000)
	ctx.openChannel(ctx.carol, ctx.eve, 135000)

	defer ctx.closeChannels()

	// Increase Dave's fee to make the test deterministic. Otherwise it
	// would be unpredictable whether pathfinding would go through Charlie
	// or Dave for the first shard.
	req := &lnrpc.PolicyUpdateRequest{
		Scope:         &lnrpc.PolicyUpdateRequest_Global{Global: true},
		BaseFeeMsat:   500000,
		FeeRate:       0.001,
		TimeLockDelta: 40,
	}
	ht.UpdateChannelPolicy(ctx.dave, req)

	// Our first test will be Alice paying Bob using a SendPayment call.
	// Let Bob create an invoice for Alice to pay.
	payReqs, rHashes, invoices := ht.CreatePayReqs(ctx.bob, paymentAmt, 1)

	rHash := rHashes[0]
	payReq := payReqs[0]

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: payReq,
		MaxParts:       10,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	payment := ht.SendPaymentAndAssert(ctx.alice, sendReq)

	// Make sure we got the preimage.
	require.Equal(ht, hex.EncodeToString(invoices[0].RPreimage),
		payment.PaymentPreimage, "preimage doesn't match")

	// Check that Alice split the payment in at least three shards. Because
	// the hand-off of the htlc to the link is asynchronous (via a mailbox),
	// there is some non-determinism in the process. Depending on whether
	// the new pathfinding round is started before or after the htlc is
	// locked into the channel, different sharding may occur. Therefore we
	// can only check if the number of shards isn't below the theoretical
	// minimum.
	succeeded := 0
	for _, htlc := range payment.Htlcs {
		if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
			succeeded++
		}
	}

	const minExpectedShards = 3
	require.GreaterOrEqual(ht, succeeded, minExpectedShards,
		"expected shards not reached")

	// Make sure Bob show the invoice as settled for the full amount.
	inv := ht.LookupInvoice(ctx.bob, rHash)

	require.EqualValues(ht, paymentAmt, inv.AmtPaidSat,
		"incorrect payment amt")

	require.Equal(ht, lnrpc.Invoice_SETTLED, inv.State,
		"Invoice not settled")

	settled := 0
	for _, htlc := range inv.Htlcs {
		if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
			settled++
		}
	}
	require.Equal(ht, succeeded, settled,
		"num of HTLCs wrong")
}

type mppTestContext struct {
	ht *lntest.HarnessTest

	// Keep a list of all our active channels.
	networkChans      []*lnrpc.ChannelPoint
	closeChannelFuncs []func()

	alice, bob, carol, dave, eve *lntest.HarnessNode
	nodes                        []*lntest.HarnessNode
}

func newMppTestContext(ht *lntest.HarnessTest) *mppTestContext {
	alice, bob := ht.Alice, ht.Bob
	ht.RestartNodeWithExtraArgs(bob, []string{"--accept-amp"})

	// Create a five-node context consisting of Alice, Bob and three new
	// nodes.
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)
	eve := ht.NewNode("eve", nil)

	// Connect nodes to ensure propagation of channels.
	nodes := []*lntest.HarnessNode{alice, bob, carol, dave, eve}
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			ht.EnsureConnected(nodes[i], nodes[j])
		}
	}

	// Send coins to the nodes and mine 2 blocks to confirm them.
	ht.SendCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, carol)
	ht.SendCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, dave)
	ht.SendCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, eve)
	ht.MineBlocks(2)

	ctx := mppTestContext{
		ht:    ht,
		alice: alice,
		bob:   bob,
		carol: carol,
		dave:  dave,
		eve:   eve,
		nodes: nodes,
	}

	return &ctx
}

// openChannel is a helper to open a channel from->to.
func (c *mppTestContext) openChannel(from, to *lntest.HarnessNode,
	chanSize btcutil.Amount) *lnrpc.ChannelPoint {

	chanPoint := c.ht.OpenChannel(
		from, to, lntest.OpenChannelParams{Amt: chanSize},
	)

	c.closeChannelFuncs = append(c.closeChannelFuncs, func() {
		c.ht.CloseChannel(from, chanPoint, false)
	})

	c.networkChans = append(c.networkChans, chanPoint)
	return chanPoint
}

func (c *mppTestContext) closeChannels() {
	if c.ht.Failed() {
		c.ht.Log("Skipped closing channels for failed test")
		return
	}

	for _, f := range c.closeChannelFuncs {
		f()
	}
}

// Helper function for Alice to build a route from pubkeys.
func (c *mppTestContext) buildRoute(amt btcutil.Amount,
	sender *lntest.HarnessNode, hops []*lntest.HarnessNode) *lnrpc.Route {

	rpcHops := make([][]byte, 0, len(hops))
	for _, hop := range hops {
		k := hop.PubKeyStr
		pubkey, err := route.NewVertexFromStr(k)
		require.NoErrorf(c.ht, err, "error parsing %v: %v", k, err)
		rpcHops = append(rpcHops, pubkey[:])
	}

	req := &routerrpc.BuildRouteRequest{
		AmtMsat:        int64(amt * 1000),
		FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
		HopPubkeys:     rpcHops,
	}

	routeResp := c.ht.BuildRoute(sender, req)

	return routeResp.Route
}
