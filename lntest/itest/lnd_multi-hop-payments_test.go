package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

func testMultiHopPayments(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100000)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	alice, bob := ht.Alice(), ht.Bob()
	chanPointAlice := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 node, 3 channel topology. Dave will make a
	// channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice. Dave will
	// be running an older node that requires the legacy onion payload.
	daveArgs := []string{"--protocol.legacy.onion"}
	dave := ht.NewNode("Dave", daveArgs)
	defer ht.Shutdown(dave)

	ht.ConnectNodes(dave, alice)
	ht.SendCoins(btcutil.SatoshiPerBitcoin, dave)

	chanPointDave := ht.OpenChannel(
		dave, alice, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Next, we'll create Carol and establish a channel to from her to
	// Dave.
	carol := ht.NewNode("Carol", nil)
	defer ht.Shutdown(carol)

	ht.ConnectNodes(carol, dave)
	ht.SendCoins(btcutil.SatoshiPerBitcoin, carol)

	chanPointCarol := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Create 5 invoices for Bob, which expect a payment from Carol for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, numPayments)

	// Set the fee policies of the Alice -> Bob and the Dave -> Alice
	// channel edges to relatively large non default values. This makes it
	// possible to pick up more subtle fee calculation errors.
	maxHtlc := calculateMaxHtlc(chanAmt)
	const aliceBaseFeeSat = 1
	const aliceFeeRatePPM = 100000
	updateChannelPolicy(
		ht, alice, chanPointAlice, aliceBaseFeeSat*1000,
		aliceFeeRatePPM, chainreg.DefaultBitcoinTimeLockDelta,
		maxHtlc, carol,
	)

	const daveBaseFeeSat = 5
	const daveFeeRatePPM = 150000
	updateChannelPolicy(
		ht, dave, chanPointDave, daveBaseFeeSat*1000, daveFeeRatePPM,
		chainreg.DefaultBitcoinTimeLockDelta, maxHtlc, carol,
	)

	// Before we start sending payments, subscribe to htlc events for each
	// node.
	aliceEvents := ht.SubscribeHtlcEvents(alice)
	bobEvents := ht.SubscribeHtlcEvents(bob)
	carolEvents := ht.SubscribeHtlcEvents(carol)
	daveEvents := ht.SubscribeHtlcEvents(dave)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ht.CompletePaymentRequests(carol, payReqs, true)

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Bob, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Carol->David->Alice->Bob, order is Bob,
	// Alice, David, Carol.

	// The final node bob expects to get paid five times 1000 sat.
	expectedAmountPaidAtoB := int64(numPayments * paymentAmt)

	ht.AssertAmountPaid("Alice(local) => Bob(remote)", bob,
		chanPointAlice, int64(0), expectedAmountPaidAtoB)
	ht.AssertAmountPaid("Alice(local) => Bob(remote)", alice,
		chanPointAlice, expectedAmountPaidAtoB, int64(0))

	// To forward a payment of 1000 sat, Alice is charging a fee of
	// 1 sat + 10% = 101 sat.
	const aliceFeePerPayment = aliceBaseFeeSat +
		(paymentAmt * aliceFeeRatePPM / 1_000_000)
	const expectedFeeAlice = numPayments * aliceFeePerPayment

	// Dave needs to pay what Alice pays plus Alice's fee.
	expectedAmountPaidDtoA := expectedAmountPaidAtoB + expectedFeeAlice

	ht.AssertAmountPaid("Dave(local) => Alice(remote)", alice,
		chanPointDave, int64(0), expectedAmountPaidDtoA)
	ht.AssertAmountPaid("Dave(local) => Alice(remote)", dave,
		chanPointDave, expectedAmountPaidDtoA, int64(0))

	// To forward a payment of 1101 sat, Dave is charging a fee of
	// 5 sat + 15% = 170.15 sat. This is rounded down in rpcserver to 170.
	const davePaymentAmt = paymentAmt + aliceFeePerPayment
	const daveFeePerPayment = daveBaseFeeSat +
		(davePaymentAmt * daveFeeRatePPM / 1_000_000)
	const expectedFeeDave = numPayments * daveFeePerPayment

	// Carol needs to pay what Dave pays plus Dave's fee.
	expectedAmountPaidCtoD := expectedAmountPaidDtoA + expectedFeeDave

	ht.AssertAmountPaid("Carol(local) => Dave(remote)", dave,
		chanPointCarol, int64(0), expectedAmountPaidCtoD)
	ht.AssertAmountPaid("Carol(local) => Dave(remote)", carol,
		chanPointCarol, expectedAmountPaidCtoD, int64(0))

	// Now that we know all the balances have been settled out properly,
	// we'll ensure that our internal record keeping for completed circuits
	// was properly updated.

	// First, check that the FeeReport response shows the proper fees
	// accrued over each time range. Dave should've earned 170 satoshi for
	// each of the forwarded payments.
	ht.AssertFeeReport(
		dave, expectedFeeDave, expectedFeeDave, expectedFeeDave,
	)

	// Next, ensure that if we issue the vanilla query for the forwarding
	// history, it returns 5 values, and each entry is formatted properly.
	fwdingHistory := ht.ForwardingHistory(dave)
	require.Len(ht, fwdingHistory.ForwardingEvents, numPayments)
	expectedForwardingFee := uint64(expectedFeeDave / numPayments)
	for _, event := range fwdingHistory.ForwardingEvents {
		// Each event should show a fee of 170 satoshi.
		require.Equal(ht, expectedForwardingFee, event.Fee)
	}

	// We expect Carol to have successful forwards and settles for
	// her sends.
	ht.AssertHtlcEvents(
		carolEvents, numPayments, 0, numPayments,
		0, routerrpc.HtlcEvent_SEND,
	)

	// Dave and Alice should both have forwards and settles for
	// their role as forwarding nodes.
	ht.AssertHtlcEvents(
		daveEvents, numPayments, 0, numPayments,
		0, routerrpc.HtlcEvent_FORWARD,
	)
	ht.AssertHtlcEvents(
		aliceEvents, numPayments, 0, numPayments,
		0, routerrpc.HtlcEvent_FORWARD,
	)

	// Bob should only have settle events for his receives.
	ht.AssertHtlcEvents(
		bobEvents, 0, 0, numPayments, 0, routerrpc.HtlcEvent_RECEIVE,
	)

	ht.CloseChannel(alice, chanPointAlice, false)
	ht.CloseChannel(dave, chanPointDave, false)
	ht.CloseChannel(carol, chanPointCarol, false)
}

// assertHtlcEvents consumes events from a client and ensures that they are of
// the expected type and contain the expected number of forwards, forward
// failures and settles.
// TODO(yy): delete
func assertHtlcEvents(t *harnessTest, fwdCount, fwdFailCount, settleCount int,
	userType routerrpc.HtlcEvent_EventType,
	client routerrpc.Router_SubscribeHtlcEventsClient) {

	var forwards, forwardFails, settles int

	numEvents := fwdCount + fwdFailCount + settleCount
	for i := 0; i < numEvents; i++ {
		event := assertEventAndType(t, userType, client)

		switch event.Event.(type) {
		case *routerrpc.HtlcEvent_ForwardEvent:
			forwards++

		case *routerrpc.HtlcEvent_ForwardFailEvent:
			forwardFails++

		case *routerrpc.HtlcEvent_SettleEvent:
			settles++

		default:
			t.Fatalf("unexpected event: %T", event.Event)
		}
	}

	if forwards != fwdCount {
		t.Fatalf("expected: %v forwards, got: %v", fwdCount, forwards)
	}

	if forwardFails != fwdFailCount {
		t.Fatalf("expected: %v forward fails, got: %v", fwdFailCount,
			forwardFails)
	}

	if settles != settleCount {
		t.Fatalf("expected: %v settles, got: %v", settleCount, settles)
	}
}

// assertEventAndType reads an event from the stream provided and ensures that
// it is associated with the correct user related type - a user initiated send,
// a receive to our node or a forward through our node. Note that this event
// type is different from the htlc event type (forward, link failure etc).
// TODO(yy): delete
func assertEventAndType(t *harnessTest, eventType routerrpc.HtlcEvent_EventType,
	client routerrpc.Router_SubscribeHtlcEventsClient) *routerrpc.HtlcEvent {

	event, err := client.Recv()
	if err != nil {
		t.Fatalf("could not get event")
	}

	if event.EventType != eventType {
		t.Fatalf("expected: %v, got: %v", eventType,
			event.EventType)
	}

	return event
}

// updateChannelPolicy updates the channel policy of node to the given fees and
// timelock delta. This function blocks until listenerNode has received the
// policy update.
//
// NOTE: only used in current test.
func updateChannelPolicy(ht *lntest.HarnessTest, hn *lntest.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, baseFee int64,
	feeRate int64, timeLockDelta uint32,
	maxHtlc uint64, listenerNode *lntest.HarnessNode) {

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      baseFee,
		FeeRateMilliMsat: feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      maxHtlc,
	}

	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate) / testFeeBase,
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
		MaxHtlcMsat: maxHtlc,
	}

	ht.UpdateChannelPolicy(hn, updateFeeReq)

	// Wait for listener node to receive the channel update from node.
	ht.AssertChannelPolicyUpdate(
		listenerNode, hn.PubKeyStr,
		expectedPolicy, chanPoint, false,
	)
}
