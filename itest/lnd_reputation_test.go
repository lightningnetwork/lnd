package itest

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

// reputationChangeLog is the greppable prefix the reputation subsystem logs
// once per resolved HTLC.
const reputationChangeLog = "Reputation change: outgoing="

// testLocalReputationLogOnly verifies that enabling the experimental,
// read-only local reputation subsystem on a forwarding node does not affect
// routing. It exercises the log-only invariant across the paths the switch
// hooks observe (a successful forward, a failed forward, and a restart),
// asserting forwarding behaviour is unchanged in every case.
//
// Beyond non-interference, it also confirms the subsystem actually computes
// reputation by matching the greppable log lines it emits: on the forward Bob
// logs the per-HTLC reputation decision, and on resolution he logs the
// resulting reputation change. After a restart, which resets the in-memory
// state, a further forward must produce another change, proving the subsystem
// rebuilt its state from live traffic.
func testLocalReputationLogOnly(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100_000)
	const paymentAmt = 1000

	// Alice -> Bob -> Carol. The read-only reputation subsystem is enabled
	// by default, so Bob (the forwarding node) runs it without any extra
	// flag.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
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

	// On the forward, Bob logs the per-HTLC reputation decision ("if this
	// HTLC were forwarded in isolation, would its outgoing channel have
	// sufficient reputation to be protected?"). Its presence confirms the
	// OnForward hook fired and the decision was computed (log-only).
	ht.AssertNodeLogContains(bob, "reputation decision: chan=")

	// On resolution Bob logs the reputation change for the outgoing
	// (Bob -> Carol) channel, confirming the subsystem observed both the
	// OnForward and the OnSettle hook and computed an update.
	ht.AssertNodeLogContains(bob, reputationChangeLog)

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

	// 3. Restart. This slice has no persistence, so restarting Bob resets
	// the in-memory reputation state; it re-accrues from live traffic (the
	// documented self-bootstrapping behaviour). Bob must come back and keep
	// forwarding.
	//
	// Record how many reputation changes have been logged so far, so that
	// the assertion after the restart can require a new one rather than
	// re-matching a line from an earlier step.
	changesBeforeRestart := ht.CountNodeLogOccurrences(
		bob, reputationChangeLog,
	)

	ht.RestartNode(bob)
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.AssertNodeNumChannels(bob, 2)
	ht.AssertChannelActive(bob, chanPointAB)
	ht.AssertChannelActive(bob, chanPointBC)

	// A subsequent payment must still forward successfully after the
	// restart, confirming the subsystem does not interfere with forwarding
	// once it has restarted with empty state.
	payReqs2, _, _ := ht.CreatePayReqs(carol, paymentAmt, 1)
	ht.CompletePaymentRequests(alice, payReqs2)

	// And reputation re-accrues from live traffic. Earlier steps already
	// logged reputation changes of their own, so asserting the line is
	// merely present would prove nothing here: we require the count to have
	// grown, which can only come from this post-restart forward being
	// observed and scored by the reset subsystem.
	ht.AssertNodeLogCountAtLeast(
		bob, reputationChangeLog, changesBeforeRestart+1,
	)

	ht.CloseChannel(alice, chanPointAB)
	ht.CloseChannel(bob, chanPointBC)
}
