package itest

import (
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
	defer ctx.shutdownNodes()

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
		paymentsResp := ht.ListPayments(node, true)

		payments := paymentsResp.Payments
		require.NotEmpty(ht, payments, "no payments found")

		payment := payments[len(payments)-1]
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
	assertSettledInvoice := func(node *lntest.HarnessNode,
		rhash []byte, num int) {

		var payHash lntypes.Hash
		copy(payHash[:], rhash)
		inv := ht.AssertInvoiceState(
			node, payHash, lnrpc.Invoice_SETTLED,
		)

		// Assert that the amount paid to the invoice is correct.
		require.EqualValues(ht, paymentAmt, inv.AmtPaidSat,
			"incorrect payment amt")

		require.Len(ht, inv.Htlcs, num, "wrong num of HTLCs")

	}

	// Finally check that the payment shows up with three settled HTLCs in
	// Alice's list of payments...
	assertNumHtlcs(ctx.alice, 3)

	// ...and in Bob's list of paid invoices.
	assertSettledInvoice(ctx.bob, rHash, 3)
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
	alice := ht.NewNode("alice", nil)
	bob := ht.NewNode("bob", []string{"--accept-amp"})

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
	chanSize btcutil.Amount) {

	c.ht.SendCoins(btcutil.SatoshiPerBitcoin, from)

	chanPoint := c.ht.OpenChannel(
		from, to, lntest.OpenChannelParams{Amt: chanSize},
	)

	c.closeChannelFuncs = append(c.closeChannelFuncs, func() {
		c.ht.CloseChannel(from, chanPoint, false)
	})

	c.networkChans = append(c.networkChans, chanPoint)
}

func (c *mppTestContext) closeChannels() {
	for _, f := range c.closeChannelFuncs {
		f()
	}
}

func (c *mppTestContext) shutdownNodes() {
	c.ht.Shutdown(c.alice)
	c.ht.Shutdown(c.bob)
	c.ht.Shutdown(c.carol)
	c.ht.Shutdown(c.dave)
	c.ht.Shutdown(c.eve)
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
