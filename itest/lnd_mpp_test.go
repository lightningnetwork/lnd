package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// testSendToRouteMultiPath tests that we are able to successfully route a
// payment using multiple shards across different paths, by using SendToRoute.
func testSendToRouteMultiPath(ht *lntest.HarnessTest) {
	mts := newMppTestScenario(ht)

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
	req := &mppOpenChannelRequest{
		// Since the channel Alice-> Carol will have to carry two
		// shards, we make it larger.
		amtAliceCarol: chanAmt + shardAmt,
		amtAliceDave:  chanAmt,
		amtCarolBob:   chanAmt,
		amtCarolEve:   chanAmt,
		amtDaveBob:    chanAmt,
		amtEveBob:     chanAmt,
	}
	mts.openChannels(req)

	// Make Bob create an invoice for Alice to pay.
	payReqs, rHashes, invoices := ht.CreatePayReqs(mts.bob, paymentAmt, 1)

	rHash := rHashes[0]
	payReq := payReqs[0]

	decodeResp := mts.bob.RPC.DecodePayReq(payReq)
	payAddr := decodeResp.PaymentAddr

	// Subscribe the invoice.
	stream := mts.bob.RPC.SubscribeSingleInvoice(rHash)

	// We'll send shards along three routes from Alice.
	sendRoutes := [][]*node.HarnessNode{
		{mts.carol, mts.bob},
		{mts.dave, mts.bob},
		{mts.carol, mts.eve, mts.bob},
	}

	responses := make(chan *lnrpc.HTLCAttempt, len(sendRoutes))
	for _, hops := range sendRoutes {
		// Build a route for the specified hops.
		r := mts.buildRoute(shardAmt, mts.alice, hops)

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
			resp := mts.alice.RPC.SendToRouteV2(sendReq)
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
	assertNumHtlcs := func(hn *node.HarnessNode, num int) {
		var preimage lntypes.Preimage
		copy(preimage[:], invoices[0].RPreimage)

		payment := ht.AssertPaymentStatus(
			hn, preimage, lnrpc.Payment_SUCCEEDED,
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
	assertNumHtlcs(mts.alice, 3)

	// ...and in Bob's list of paid invoices.
	assertSettledInvoice(rHash, 3)

	// Finally, close all channels.
	mts.closeChannels()
}

// mppTestScenario defines a test scenario used for testing MPP-related tests.
// It has two standby nodes, alice and bob, and three new nodes, carol, dave,
// and eve.
type mppTestScenario struct {
	ht *lntest.HarnessTest

	alice, bob, carol, dave, eve *node.HarnessNode
	nodes                        []*node.HarnessNode

	// Keep a list of all our active channels.
	channelPoints []*lnrpc.ChannelPoint
}

// newMppTestScenario initializes a new mpp test scenario with five funded
// nodes and connects them to have the following topology,
//
//	            _ Eve _
//	           /       \
//	Alice -- Carol ---- Bob
//	    \              /
//	     \__ Dave ____/
func newMppTestScenario(ht *lntest.HarnessTest) *mppTestScenario {
	alice, bob := ht.Alice, ht.Bob
	ht.RestartNodeWithExtraArgs(bob, []string{
		"--maxpendingchannels=2",
		"--accept-amp",
	})

	// Create a five-node context consisting of Alice, Bob and three new
	// nodes.
	carol := ht.NewNode("carol", []string{
		"--maxpendingchannels=2",
		"--accept-amp",
	})
	dave := ht.NewNode("dave", nil)
	eve := ht.NewNode("eve", nil)

	// Connect nodes to ensure propagation of channels.
	ht.EnsureConnected(alice, carol)
	ht.EnsureConnected(alice, dave)
	ht.EnsureConnected(carol, bob)
	ht.EnsureConnected(carol, eve)
	ht.EnsureConnected(dave, bob)
	ht.EnsureConnected(eve, bob)

	// Send coins to the nodes and mine 1 blocks to confirm them.
	for i := 0; i < 2; i++ {
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, carol)
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, dave)
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, eve)
		ht.MineBlocks(1)
	}

	mts := &mppTestScenario{
		ht:    ht,
		alice: alice,
		bob:   bob,
		carol: carol,
		dave:  dave,
		eve:   eve,
		nodes: []*node.HarnessNode{alice, bob, carol, dave, eve},
	}

	return mts
}

// mppOpenChannelRequest defines the amounts used for each channel opening.
type mppOpenChannelRequest struct {
	// Channel Alice=>Carol.
	amtAliceCarol btcutil.Amount

	// Channel Alice=>Dave.
	amtAliceDave btcutil.Amount

	// Channel Carol=>Bob.
	amtCarolBob btcutil.Amount

	// Channel Carol=>Eve.
	amtCarolEve btcutil.Amount

	// Channel Dave=>Bob.
	amtDaveBob btcutil.Amount

	// Channel Eve=>Bob.
	amtEveBob btcutil.Amount
}

// openChannels is a helper to open channels that sets up a network topology
// with three different paths Alice <-> Bob as following,
//
//		 _ Eve _
//		/       \
//	 Alice -- Carol ---- Bob
//		\              /
//		 \__ Dave ____/
//
// NOTE: all the channels are open together to save blocks mined.
func (m *mppTestScenario) openChannels(r *mppOpenChannelRequest) {
	reqs := []*lntest.OpenChannelRequest{
		{
			Local:  m.alice,
			Remote: m.carol,
			Param:  lntest.OpenChannelParams{Amt: r.amtAliceCarol},
		},
		{
			Local:  m.alice,
			Remote: m.dave,
			Param:  lntest.OpenChannelParams{Amt: r.amtAliceDave},
		},
		{
			Local:  m.carol,
			Remote: m.bob,
			Param:  lntest.OpenChannelParams{Amt: r.amtCarolBob},
		},
		{
			Local:  m.carol,
			Remote: m.eve,
			Param:  lntest.OpenChannelParams{Amt: r.amtCarolEve},
		},
		{
			Local:  m.dave,
			Remote: m.bob,
			Param:  lntest.OpenChannelParams{Amt: r.amtDaveBob},
		},
		{
			Local:  m.eve,
			Remote: m.bob,
			Param:  lntest.OpenChannelParams{Amt: r.amtEveBob},
		},
	}

	m.channelPoints = m.ht.OpenMultiChannelsAsync(reqs)

	// Make sure every node has heard every channel.
	for _, hn := range m.nodes {
		for _, cp := range m.channelPoints {
			m.ht.AssertTopologyChannelOpen(hn, cp)
		}

		// Each node should have exactly 6 edges.
		m.ht.AssertNumEdges(hn, len(m.channelPoints), false)
	}
}

// closeChannels closes all the open channels from `openChannels`.
func (m *mppTestScenario) closeChannels() {
	if m.ht.Failed() {
		m.ht.Log("Skipped closing channels for failed test")
		return
	}

	// TODO(yy): remove the sleep once the following bug is fixed. When the
	// payment is reported as settled by Alice, it's expected the
	// commitment dance is finished and all subsequent states have been
	// updated. Yet we'd receive the error `cannot co-op close channel with
	// active htlcs` or `link failed to shutdown` if we close the channel.
	// We need to investigate the order of settling the payments and
	// updating commitments to understand and fix .
	time.Sleep(5 * time.Second)

	// Close all channels without mining the closing transactions.
	m.ht.CloseChannelAssertPending(m.alice, m.channelPoints[0], false)
	m.ht.CloseChannelAssertPending(m.alice, m.channelPoints[1], false)
	m.ht.CloseChannelAssertPending(m.carol, m.channelPoints[2], false)
	m.ht.CloseChannelAssertPending(m.carol, m.channelPoints[3], false)
	m.ht.CloseChannelAssertPending(m.dave, m.channelPoints[4], false)
	m.ht.CloseChannelAssertPending(m.eve, m.channelPoints[5], false)

	// Now mine a block to include all the closing transactions.
	m.ht.MineBlocks(1)

	// Assert that the channels are closed.
	for _, hn := range m.nodes {
		m.ht.AssertNumWaitingClose(hn, 0)
	}
}

// Helper function for Alice to build a route from pubkeys.
func (m *mppTestScenario) buildRoute(amt btcutil.Amount,
	sender *node.HarnessNode, hops []*node.HarnessNode) *lnrpc.Route {

	rpcHops := make([][]byte, 0, len(hops))
	for _, hop := range hops {
		k := hop.PubKeyStr
		pubkey, err := route.NewVertexFromStr(k)
		require.NoErrorf(m.ht, err, "error parsing %v: %v", k, err)
		rpcHops = append(rpcHops, pubkey[:])
	}

	req := &routerrpc.BuildRouteRequest{
		AmtMsat:        int64(amt * 1000),
		FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
		HopPubkeys:     rpcHops,
	}
	routeResp := sender.RPC.BuildRoute(req)

	return routeResp.Route
}

// updatePolicy updates a Dave's global channel policy and returns the expected
// policy for further check. It changes Dave's `FeeBaseMsat` from 1000 msat to
// 500,000 msat, and `FeeProportionalMillonths` from 1 msat to 1000 msat.
func (m *mppTestScenario) updateDaveGlobalPolicy() *lnrpc.RoutingPolicy {
	const (
		baseFeeMsat = 500_000
		feeRate     = 0.001
		maxHtlcMsat = 133_650_000
	)

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      baseFeeMsat,
		FeeRateMilliMsat: feeRate * testFeeBase,
		TimeLockDelta:    40,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      maxHtlcMsat,
	}

	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFeeMsat,
		FeeRate:       feeRate,
		TimeLockDelta: 40,
		Scope:         &lnrpc.PolicyUpdateRequest_Global{Global: true},
		MaxHtlcMsat:   maxHtlcMsat,
	}
	m.dave.RPC.UpdateChannelPolicy(updateFeeReq)

	return expectedPolicy
}
