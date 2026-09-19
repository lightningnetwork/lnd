package itest

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testIntervalRouterMultiHopPayment tests the experimental interval router end
// to end over a three hop network, with only the sender running it so that the
// test also covers a node routing for peers that do not.
//
// The payment the router is interesting on is the second one. We drain the
// middle hop so that a large payment cannot get through, watch it fail at that
// hop, and then send a small one without resetting anything. The stock router
// would have penalized the node pair and would need the penalty to decay; the
// interval router records the amount that failed as a bound, which leaves every
// smaller amount over the same channel perfectly routable. The retry is
// therefore a test of what the router remembered, not of what it forgot.
func testIntervalRouterMultiHopPayment(ht *lntest.HarnessTest) {
	const (
		chanAmt = btcutil.Amount(300_000)

		// smallPayment fits comfortably inside what is left of the
		// middle hop after the drain below.
		smallPayment = btcutil.Amount(10_000)

		// largePayment is well above it.
		largePayment = btcutil.Amount(100_000)

		// drainPayment leaves the middle hop with roughly 50k of
		// outbound liquidity, less its reserve and fee buffer.
		drainPayment = btcutil.Amount(250_000)
	)

	// Build Alice -> Bob -> Carol, with only Alice routing on intervals.
	cfgs := [][]string{{"--routerrpc.router=interval"}, nil, nil}
	_, nodes := ht.CreateSimpleNetwork(
		cfgs, lntest.OpenChannelParams{Amt: chanAmt},
	)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]

	// sendPayment is a helper that pays a fresh invoice of the given amount
	// from Alice to Carol over a single HTLC, so that the outcome is about
	// route choice rather than about how the payment was split.
	sendPayment := func(amt btcutil.Amount) *routerrpc.SendPaymentRequest {
		invoice := carol.RPC.AddInvoice(&lnrpc.Invoice{
			Value: int64(amt),
		})

		return &routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
			MaxParts:       1,
		}
	}

	// With liquidity everywhere, the interval router finds the two hop
	// route and the payment settles.
	payment := ht.SendPaymentAssertSettled(alice, sendPayment(smallPayment))
	require.Len(ht, payment.Htlcs, 1)
	require.Len(ht, payment.Htlcs[0].Route.Hops, 2)
	require.Equal(
		ht, carol.PubKeyStr,
		payment.Htlcs[0].Route.Hops[1].PubKey,
	)

	// Now drain the middle hop by having Bob pay Carol, which leaves Bob
	// without the outbound liquidity to forward a large payment onwards.
	drainInvoice := carol.RPC.AddInvoice(&lnrpc.Invoice{
		Value: int64(drainPayment),
	})
	ht.SendPaymentAssertSettled(bob, &routerrpc.SendPaymentRequest{
		PaymentRequest: drainInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	})

	// Alice cannot know that from the graph, so she tries the large payment
	// and Bob refuses to forward it. With no second route to Carol and no
	// room to split, the payment fails.
	ht.SendPaymentAssertFail(
		alice, sendPayment(largePayment),
		lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE,
	)
	ht.AssertLastHTLCError(alice, lnrpc.Failure_TEMPORARY_CHANNEL_FAILURE)

	// The failure told Alice's router an amount, not a verdict on the
	// channel. Nothing is reset here, and no penalty is waited out: a
	// smaller payment over the very same hop settles immediately, because
	// the bound the router recorded rules out the amount that failed and
	// says nothing against this one.
	payment = ht.SendPaymentAssertSettled(alice, sendPayment(smallPayment))
	require.Len(ht, payment.Htlcs, 1)
	require.Len(ht, payment.Htlcs[0].Route.Hops, 2)
	require.Equal(
		ht, bob.PubKeyStr, payment.Htlcs[0].Route.Hops[0].PubKey,
	)

	// The whole of that recovery took one attempt, since the router asked
	// for an amount it had no evidence against rather than retrying the one
	// it had just watched fail.
	require.Equal(
		ht, lnrpc.HTLCAttempt_SUCCEEDED, payment.Htlcs[0].Status,
	)
}
