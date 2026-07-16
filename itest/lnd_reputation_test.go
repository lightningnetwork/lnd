package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
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
// switch hooks observe — a successful forward, a failed forward, and a restart
// — asserting forwarding behaviour is unchanged in every case.
//
// Beyond non-interference, it also verifies the subsystem actually computes
// reputation: on the forward Bob logs the per-HTLC isolation decision, and
// after the successful forward settles his log must contain the "Reputation
// gained" summary (emitted only when the resolution yields a positive effective
// fee). It then reads the exact computed values back over the read-only devrpc
// FetchReputation RPC: the outgoing (Bob->Carol) channel's reputation increased
// by exactly the forwarding fee Bob earned, and the incoming (Alice->Bob)
// channel's revenue moved to a positive value.
func testLocalReputationLogOnly(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100_000)
	const paymentAmt = 1000

	// Alice -> Bob -> Carol. The read-only reputation subsystem is enabled
	// by default, so Bob (the forwarding node) runs it without any extra
	// flag; we inspect its reputation state below.
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

	// On the forward, Bob logs the per-HTLC isolation decision ("if this
	// HTLC were forwarded in isolation, would its outgoing channel have
	// sufficient reputation to be protected?"). Its presence confirms the
	// OnForward hook fired and the decision was computed (log-only).
	ht.AssertNodeLogContains(bob, "reputation decision: chan=")

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
	// earned. This is deterministic: on a fresh channel a single fast
	// settle within the resolution period yields effective_fee ==
	// advertised fee, so the decaying average holds exactly that value at
	// read time.
	require.Equal(ht, feeMsat, outRep.OutgoingReputation,
		"outgoing reputation must equal the forwarding fee")

	// The incoming channel's revenue moved to a positive value. We assert
	// only positivity, not an exact figure: the revenue is read through the
	// aggregated-window average's exponential warm-up divisor, whose value
	// depends on how much wall-clock time has elapsed since the channel's
	// first observation at read time, so the exact integer is not stable.
	require.Positive(ht, inRep.IncomingRevenue,
		"incoming revenue must be positive after a settled forward")

	// The HTLC has fully resolved, so pending state must be back to zero on
	// every channel.
	for _, cr := range rep.ChannelReputations {
		require.Zerof(ht, cr.PendingHtlcCount,
			"scid %d has pending HTLCs after settle", cr.Scid)
		require.Zerof(ht, cr.InFlightRisk,
			"scid %d has in-flight risk after settle", cr.Scid)
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

	// 3. Restart. This slice has no persistence, so restarting Bob resets
	// the in-memory reputation state; it re-accrues from live traffic (the
	// documented self-bootstrapping behaviour). Bob must come back and keep
	// forwarding.
	ht.RestartNode(bob)
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.AssertNodeNumChannels(bob, 2)
	ht.AssertChannelActive(bob, chanPointAB)
	ht.AssertChannelActive(bob, chanPointBC)

	// The subsystem is not persisted, so immediately after the restart
	// Bob's snapshot must be empty: reputation is re-accrued from live
	// traffic only. This confirms the documented self-bootstrapping reset.
	repAfterRestart := bob.RPC.FetchReputation(
		&devrpc.FetchReputationRequest{},
	)
	require.Empty(ht, repAfterRestart.ChannelReputations,
		"reputation snapshot must be empty after a restart (no "+
			"persistence)")

	// A subsequent payment must still forward successfully after the
	// restart, confirming the subsystem does not interfere with forwarding
	// once it has restarted with empty state.
	payReqs2, _, _ := ht.CreatePayReqs(carol, paymentAmt, 1)
	ht.CompletePaymentRequests(alice, payReqs2)

	// And reputation re-accrues from live traffic: after this second
	// forward+settle the outgoing channel again shows a positive
	// reputation, proving the reset subsystem rebuilt its state from
	// scratch.
	ht.AssertNodeLogContains(bob, "Reputation gained: outgoing=")
	err = wait.NoError(func() error {
		repRebuilt := bob.RPC.FetchReputation(
			&devrpc.FetchReputationRequest{},
		)
		for _, cr := range repRebuilt.ChannelReputations {
			if cr.Scid != outgoingSCID {
				continue
			}
			if cr.OutgoingReputation > 0 {
				return nil
			}

			return fmt.Errorf("outgoing reputation not "+
				"re-accrued: got %d", cr.OutgoingReputation)
		}

		return fmt.Errorf("no snapshot for outgoing scid %d yet",
			outgoingSCID)
	}, defaultTimeout)
	require.NoError(ht, err, "reputation did not re-accrue after restart")

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
