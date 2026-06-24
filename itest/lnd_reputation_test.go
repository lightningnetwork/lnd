package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
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
// positive effective fee), and the devrpc FetchReputation read-out must report
// the exact computed values — the outgoing (Bob->Carol) channel's reputation
// increased by exactly the forwarding fee Bob earned, the incoming (Alice->Bob)
// channel's revenue moved by the same fee, and all bucket occupancy returned to
// zero once the HTLC settled.
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
	// observed the forward+settle and computed a real, positive update. It
	// also serves as a synchronization point: once it appears, Bob has
	// observed both the OnForward and the OnSettle hook, so the reputation
	// snapshot below reflects the fully-resolved HTLC.
	ht.AssertNodeLogContains(bob, "Reputation gained: outgoing=")

	// Now assert the exact computed values via the read-only devrpc
	// FetchReputation RPC. First, resolve the short channel ids of Bob's
	// two channels from Bob's own view, and read the exact fee Bob earned
	// on the forward from his forwarding history (this is the ground truth:
	// for an unaccountable HTLC that settles within the resolution period,
	// the effective fee equals the full forwarding fee).
	bobChanAB := ht.QueryChannelByChanPoint(bob, chanPointAB)
	bobChanBC := ht.QueryChannelByChanPoint(bob, chanPointBC)
	incomingSCID := bobChanAB.ChanId
	outgoingSCID := bobChanBC.ChanId

	// Forwarding events are flushed to disk on a periodic ticker, so the
	// event may not be queryable the instant the payment settles. Poll
	// until exactly one event is reported before reading the fee.
	var feeMsat int64
	err := wait.NoError(func() error {
		fwdHistory := bob.RPC.ForwardingHistory(nil)
		if len(fwdHistory.ForwardingEvents) != 1 {
			return fmt.Errorf("expected exactly one forwarding "+
				"event, got %d",
				len(fwdHistory.ForwardingEvents))
		}

		feeMsat = int64(fwdHistory.ForwardingEvents[0].FeeMsat)

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "forwarding event not reported in time")
	require.Positive(ht, feeMsat, "forwarding fee must be positive")

	// Fetch Bob's reputation snapshot and assert the exact per-channel
	// state.
	rep := bob.RPC.FetchReputation(&devrpc.FetchReputationRequest{})

	outRep := reputationForSCID(ht, rep, outgoingSCID)
	inRep := reputationForSCID(ht, rep, incomingSCID)

	// The outgoing channel's reputation increased by exactly the fee
	// earned.
	require.Equal(ht, feeMsat, outRep.OutgoingReputation,
		"outgoing reputation must equal the forwarding fee")

	// The incoming channel's revenue moved by exactly the same fee.
	require.Equal(ht, feeMsat, inRep.IncomingRevenue,
		"incoming revenue must equal the forwarding fee")

	// The HTLC has fully resolved, so all bucket occupancy and pending
	// state must be back to zero on every channel.
	for _, cr := range rep.ChannelReputations {
		require.Zerof(ht, cr.PendingHtlcCount,
			"scid %d has pending HTLCs after settle", cr.Scid)
		require.Zerof(ht, cr.InFlightRisk,
			"scid %d has in-flight risk after settle", cr.Scid)
		require.Zerof(ht, cr.General.SlotsUsed,
			"scid %d general slots not released", cr.Scid)
		require.Zerof(ht, cr.General.LiquidityUsedMsat,
			"scid %d general liquidity not released", cr.Scid)
		require.Zerof(ht, cr.Congestion.SlotsUsed,
			"scid %d congestion slots not released", cr.Scid)
		require.Zerof(ht, cr.Protected.SlotsUsed,
			"scid %d protected slots not released", cr.Scid)
	}

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

	// Remember the exact reputation values computed before the restart so
	// we can confirm they survive it.
	preRestartOutRep := outRep.OutgoingReputation
	preRestartInRevenue := inRep.IncomingRevenue

	// 3. Warm restart. Restarting Bob reloads persisted reputation
	// state and discards any in-flight HTLC tracking (the documented
	// log-only behaviour). Bob must come back and keep forwarding.
	ht.RestartNode(bob)
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.AssertNodeNumChannels(bob, 2)
	ht.AssertChannelActive(bob, chanPointAB)
	ht.AssertChannelActive(bob, chanPointBC)

	// The decaying-average reputation state is persisted, so after the warm
	// restart FetchReputation must report the same non-zero values it did
	// before — proving the state was reloaded from disk and recomputed,
	// rather than re-derived from a log line that merely survived rotation.
	// The decay window is many weeks, so over a sub-minute restart the
	// values are unchanged up to integer rounding; allow a one-unit delta.
	repAfter := bob.RPC.FetchReputation(&devrpc.FetchReputationRequest{})
	outRepAfter := reputationForSCID(ht, repAfter, outgoingSCID)
	inRepAfter := reputationForSCID(ht, repAfter, incomingSCID)

	require.Positive(ht, outRepAfter.OutgoingReputation,
		"outgoing reputation did not persist across restart")
	require.InDelta(ht, preRestartOutRep, outRepAfter.OutgoingReputation, 1,
		"outgoing reputation changed across restart")
	require.InDelta(ht, preRestartInRevenue, inRepAfter.IncomingRevenue, 1,
		"incoming revenue changed across restart")

	// A subsequent payment must still forward successfully after the warm
	// restart, confirming the subsystem does not interfere with forwarding
	// once it has reloaded its persisted state.
	payReqs2, _, _ := ht.CreatePayReqs(carol, paymentAmt, 1)
	ht.CompletePaymentRequests(alice, payReqs2)

	ht.CloseChannel(alice, chanPointAB)
	ht.CloseChannel(bob, chanPointBC)
}

// reputationForSCID returns the per-channel reputation snapshot for the given
// short channel id, failing the test if it is absent.
func reputationForSCID(ht *lntest.HarnessTest,
	resp *devrpc.FetchReputationResponse,
	scid uint64) *devrpc.ChannelReputation {

	for _, cr := range resp.ChannelReputations {
		if cr.Scid == scid {
			return cr
		}
	}

	require.Failf(ht, "no reputation snapshot", "scid %d not found", scid)

	return nil
}
