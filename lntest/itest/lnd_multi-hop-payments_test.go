package itest

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

func testMultiHopPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = btcutil.Amount(100000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 node, 3 channel topology. Dave will make a
	// channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice. Dave will
	// be running an older node that requires the legacy onion payload.
	daveArgs := []string{"--protocol.legacy.onion"}
	dave, err := net.NewNode("Dave", daveArgs)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Bob, which expect a payment from Carol for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	time.Sleep(time.Millisecond * 50)

	// Set the fee policies of the Alice -> Bob and the Dave -> Alice
	// channel edges to relatively large non default values. This makes it
	// possible to pick up more subtle fee calculation errors.
	maxHtlc := calculateMaxHtlc(chanAmt)
	const aliceBaseFeeSat = 1
	const aliceFeeRatePPM = 100000
	updateChannelPolicy(
		t, net.Alice, chanPointAlice, aliceBaseFeeSat*1000,
		aliceFeeRatePPM, chainreg.DefaultBitcoinTimeLockDelta, maxHtlc,
		carol,
	)

	const daveBaseFeeSat = 5
	const daveFeeRatePPM = 150000
	updateChannelPolicy(
		t, dave, chanPointDave, daveBaseFeeSat*1000, daveFeeRatePPM,
		chainreg.DefaultBitcoinTimeLockDelta, maxHtlc, carol,
	)

	// Before we start sending payments, subscribe to htlc events for each
	// node.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	aliceEvents, err := net.Alice.RouterClient.SubscribeHtlcEvents(
		ctxt, &routerrpc.SubscribeHtlcEventsRequest{},
	)
	if err != nil {
		t.Fatalf("could not subscribe events: %v", err)
	}

	bobEvents, err := net.Bob.RouterClient.SubscribeHtlcEvents(
		ctxt, &routerrpc.SubscribeHtlcEventsRequest{},
	)
	if err != nil {
		t.Fatalf("could not subscribe events: %v", err)
	}

	carolEvents, err := carol.RouterClient.SubscribeHtlcEvents(
		ctxt, &routerrpc.SubscribeHtlcEventsRequest{},
	)
	if err != nil {
		t.Fatalf("could not subscribe events: %v", err)
	}

	daveEvents, err := dave.RouterClient.SubscribeHtlcEvents(
		ctxt, &routerrpc.SubscribeHtlcEventsRequest{},
	)
	if err != nil {
		t.Fatalf("could not subscribe events: %v", err)
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, carol, carol.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Bob, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Carol->David->Alice->Bob, order is Bob,
	// Alice, David, Carol.

	// The final node bob expects to get paid five times 1000 sat.
	expectedAmountPaidAtoB := int64(numPayments * paymentAmt)

	assertAmountPaid(t, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), expectedAmountPaidAtoB)
	assertAmountPaid(t, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, expectedAmountPaidAtoB, int64(0))

	// To forward a payment of 1000 sat, Alice is charging a fee of
	// 1 sat + 10% = 101 sat.
	const aliceFeePerPayment = aliceBaseFeeSat +
		(paymentAmt * aliceFeeRatePPM / 1_000_000)
	const expectedFeeAlice = numPayments * aliceFeePerPayment

	// Dave needs to pay what Alice pays plus Alice's fee.
	expectedAmountPaidDtoA := expectedAmountPaidAtoB + expectedFeeAlice

	assertAmountPaid(t, "Dave(local) => Alice(remote)", net.Alice,
		daveFundPoint, int64(0), expectedAmountPaidDtoA)
	assertAmountPaid(t, "Dave(local) => Alice(remote)", dave,
		daveFundPoint, expectedAmountPaidDtoA, int64(0))

	// To forward a payment of 1101 sat, Dave is charging a fee of
	// 5 sat + 15% = 170.15 sat. This is rounded down in rpcserver to 170.
	const davePaymentAmt = paymentAmt + aliceFeePerPayment
	const daveFeePerPayment = daveBaseFeeSat +
		(davePaymentAmt * daveFeeRatePPM / 1_000_000)
	const expectedFeeDave = numPayments * daveFeePerPayment

	// Carol needs to pay what Dave pays plus Dave's fee.
	expectedAmountPaidCtoD := expectedAmountPaidDtoA + expectedFeeDave

	assertAmountPaid(t, "Carol(local) => Dave(remote)", dave,
		carolFundPoint, int64(0), expectedAmountPaidCtoD)
	assertAmountPaid(t, "Carol(local) => Dave(remote)", carol,
		carolFundPoint, expectedAmountPaidCtoD, int64(0))

	// Now that we know all the balances have been settled out properly,
	// we'll ensure that our internal record keeping for completed circuits
	// was properly updated.

	// First, check that the FeeReport response shows the proper fees
	// accrued over each time range. Dave should've earned 170 satoshi for
	// each of the forwarded payments.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	feeReport, err := dave.FeeReport(ctxt, &lnrpc.FeeReportRequest{})
	if err != nil {
		t.Fatalf("unable to query for fee report: %v", err)
	}

	if feeReport.DayFeeSum != uint64(expectedFeeDave) {
		t.Fatalf("fee mismatch: expected %v, got %v", expectedFeeDave,
			feeReport.DayFeeSum)
	}
	if feeReport.WeekFeeSum != uint64(expectedFeeDave) {
		t.Fatalf("fee mismatch: expected %v, got %v", expectedFeeDave,
			feeReport.WeekFeeSum)
	}
	if feeReport.MonthFeeSum != uint64(expectedFeeDave) {
		t.Fatalf("fee mismatch: expected %v, got %v", expectedFeeDave,
			feeReport.MonthFeeSum)
	}

	// Next, ensure that if we issue the vanilla query for the forwarding
	// history, it returns 5 values, and each entry is formatted properly.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	fwdingHistory, err := dave.ForwardingHistory(
		ctxt, &lnrpc.ForwardingHistoryRequest{},
	)
	if err != nil {
		t.Fatalf("unable to query for fee report: %v", err)
	}
	if len(fwdingHistory.ForwardingEvents) != numPayments {
		t.Fatalf("wrong number of forwarding event: expected %v, "+
			"got %v", numPayments,
			len(fwdingHistory.ForwardingEvents))
	}
	expectedForwardingFee := uint64(expectedFeeDave / numPayments)
	for _, event := range fwdingHistory.ForwardingEvents {
		// Each event should show a fee of 170 satoshi.
		if event.Fee != expectedForwardingFee {
			t.Fatalf("fee mismatch:  expected %v, got %v",
				expectedForwardingFee, event.Fee)
		}
	}

	// We expect Carol to have successful forwards and settles for
	// her sends.
	assertHtlcEvents(
		t, numPayments, 0, numPayments, routerrpc.HtlcEvent_SEND,
		carolEvents,
	)

	// Dave and Alice should both have forwards and settles for
	// their role as forwarding nodes.
	assertHtlcEvents(
		t, numPayments, 0, numPayments, routerrpc.HtlcEvent_FORWARD,
		daveEvents,
	)
	assertHtlcEvents(
		t, numPayments, 0, numPayments, routerrpc.HtlcEvent_FORWARD,
		aliceEvents,
	)

	// Bob should only have settle events for his receives.
	assertHtlcEvents(
		t, 0, 0, numPayments, routerrpc.HtlcEvent_RECEIVE, bobEvents,
	)

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// assertHtlcEvents consumes events from a client and ensures that they are of
// the expected type and contain the expected number of forwards, forward
// failures and settles.
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
