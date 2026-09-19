package itest

import (
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testAttributableFailureHoldTimes verifies that when a payment fails at a
// downstream node, the sender receives hold times in the failure response via
// attributable errors. It sets up Alice -> Bob -> Carol, triggers a failure at
// Carol (unknown payment hash), and checks that Alice's payment result includes
// hold times that correctly correspond to the route hops.
func testAttributableFailureHoldTimes(ht *lntest.HarnessTest) {
	const chanAmt = funding.MaxBtcFundingAmount

	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	carol := ht.NewNode("Carol", nil)

	ht.EnsureConnected(alice, bob)
	ht.ConnectNodes(bob, carol)

	// Open channels: Alice -> Bob -> Carol.
	chanPointAB := ht.OpenChannel(
		alice, bob,
		lntest.OpenChannelParams{Amt: chanAmt},
	)
	chanPointBC := ht.OpenChannel(
		bob, carol,
		lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Wait for Alice to see both channels.
	ht.AssertChannelInGraph(alice, chanPointAB)
	ht.AssertChannelInGraph(alice, chanPointBC)

	// Create an invoice from Carol to get valid route parameters.
	carolInvoice := carol.RPC.AddInvoice(&lnrpc.Invoice{
		Memo:  "hold-time-test",
		Value: 10_000,
	})
	carolPayReq := carol.RPC.DecodePayReq(carolInvoice.PaymentRequest)

	// Send a payment with a random (wrong) payment hash so Carol rejects
	// it with INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS. This ensures the error
	// originates at Carol (the final hop) and propagates back through Bob
	// to Alice, with each hop adding its hold time.
	sendReq := &routerrpc.SendPaymentRequest{
		PaymentHash:    ht.Random32Bytes(),
		Dest:           carol.PubKey[:],
		Amt:            10_000,
		FinalCltvDelta: int32(carolPayReq.CltvExpiry),
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}

	payment := ht.SendPaymentAssertFail(
		alice, sendReq,
		lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS, //nolint:ll
	)

	// Verify we got at least one HTLC attempt with failure info.
	require.NotEmpty(ht, payment.Htlcs, "expected at least one HTLC")
	htlcAttempt := payment.Htlcs[len(payment.Htlcs)-1]
	require.NotNil(ht, htlcAttempt.Failure, "expected failure info")

	// The route should have 2 hops: Alice->Bob->Carol.
	require.Len(ht, htlcAttempt.Route.Hops, 2,
		"expected 2-hop route (Bob, Carol)")

	// Verify the failure code and source. Carol is at index 2 (0=Alice,
	// 1=Bob, 2=Carol).
	require.Equal(ht,
		lnrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS,
		htlcAttempt.Failure.Code,
	)
	require.EqualValues(ht, 2, htlcAttempt.Failure.FailureSourceIndex,
		"failure should originate from Carol (index 2)")

	// Verify hold times. With attributable errors, we expect one hold time
	// entry per hop in the route. hold_times[0] corresponds to
	// route.hops[0] (Bob) and hold_times[1] to route.hops[1] (Carol).
	holdTimes := htlcAttempt.Failure.HoldTimes
	require.Len(ht, holdTimes, len(htlcAttempt.Route.Hops),
		"hold_times should have one entry per route hop")

	// Hold times are in 100ms units. In a test environment with local
	// nodes, processing should be nearly instant (likely 0-2 units).
	// We verify each value is within a reasonable upper bound (< 10s)
	// to catch any corruption or mis-encoding.
	const maxReasonableHoldTime = uint32(100) // 10 seconds
	for i, holdTime := range holdTimes {
		require.LessOrEqual(ht, holdTime, maxReasonableHoldTime,
			"hold time for hop %d (%s) unreasonably large: "+
				"%d (= %dms)",
			i, htlcAttempt.Route.Hops[i].PubKey,
			holdTime, holdTime*100,
		)
	}
}
