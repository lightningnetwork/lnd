package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

// testLocalReputationLogOnly verifies that enabling the experimental,
// read-only local reputation subsystem on a forwarding node does not affect
// routing. It exercises the end-to-end log-only invariant across the paths the
// switch hooks observe — a successful forward, a failed forward, and a warm
// restart — asserting forwarding behaviour is unchanged in every case.
//
// Beyond non-interference, it also verifies the subsystem actually computes
// reputation: after a successful forward, Bob's log must contain the
// "Reputation gained" summary (emitted only when the resolution yields a
// positive effective fee), confirming the hooks fed a real, positive
// reputation update. (Asserting exact per-channel values over RPC is deferred
// to the P10 debug read-out; the log assertion gives behavioural coverage now.)
func testLocalReputationLogOnly(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100_000)
	const paymentAmt = 1000

	// Alice -> Bob -> Carol, with Bob (the forwarding node) running the
	// experimental read-only reputation subsystem.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", []string{"--routing.reputation"})
	carol := ht.NewNode("Carol", nil)

	ht.ConnectNodes(alice, bob)
	ht.ConnectNodes(bob, carol)

	// Open Alice -> Bob and Bob -> Carol.
	chanPointAB := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)
	chanPointBC := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Make sure Alice has learned of the Bob -> Carol channel so she can
	// route the multi-hop payment.
	ht.AssertChannelInGraph(alice, chanPointBC)

	// 1. Successful forward. Carol invoices, Alice pays via Bob. With Bob's
	// reputation subsystem in log-only mode this must succeed exactly as it
	// would without it (Bob observes OnForward + OnSettle).
	payReqs, _, _ := ht.CreatePayReqs(carol, paymentAmt, 1)
	ht.CompletePaymentRequests(alice, payReqs)

	// The forward earned Bob a fee, which his reputation subsystem must
	// record as a positive reputation gain for the outgoing (Bob -> Carol)
	// channel. The "Reputation gained" summary is logged at Info only when
	// the effective fee is positive, so its presence confirms the subsystem
	// observed the forward+settle and computed a real, positive update.
	ht.AssertNodeLogContains(bob, "Reputation gained: outgoing=")

	// 2. Failed forward. A payment to Carol with an unknown payment hash is
	// routed Alice -> Bob -> Carol and rejected at Carol, so Bob observes
	// the forward and its downstream failure (OnFail). Bob must remain
	// unaffected and the payment must fail cleanly.
	failReq := &routerrpc.SendPaymentRequest{
		Dest:           carol.PubKey[:],
		Amt:            paymentAmt,
		PaymentHash:    ht.Random32Bytes(),
		FinalCltvDelta: finalCltvDelta,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAssertFail(
		alice, failReq,
		lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS, //nolint:ll
	)

	// 3. Warm restart. Restarting Bob reloads persisted reputation
	// state and discards any in-flight HTLC tracking (the documented
	// log-only behaviour). Bob must come back and keep forwarding.
	ht.RestartNode(bob)
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.AssertNodeNumChannels(bob, 2)
	ht.AssertChannelActive(bob, chanPointAB)
	ht.AssertChannelActive(bob, chanPointBC)

	// A subsequent payment must still forward successfully after the warm
	// restart, confirming the subsystem does not interfere with forwarding
	// once it has reloaded its persisted state.
	payReqs2, _, _ := ht.CreatePayReqs(carol, paymentAmt, 1)
	ht.CompletePaymentRequests(alice, payReqs2)

	ht.CloseChannel(alice, chanPointAB)
	ht.CloseChannel(bob, chanPointBC)
}
