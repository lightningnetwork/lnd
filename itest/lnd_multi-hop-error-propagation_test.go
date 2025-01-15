package itest

import (
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func testHtlcErrorPropagation(ht *lntest.HarnessTest) {
	// In this test we wish to exercise the daemon's correct parsing,
	// handling, and propagation of errors that occur while processing a
	// multi-hop payment.
	const chanAmt = funding.MaxBtcFundingAmount

	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	ht.EnsureConnected(alice, bob)

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol := ht.NewNode("Carol", nil)
	ht.ConnectNodes(bob, carol)

	// Before we start sending payments, subscribe to htlc events for each
	// node.
	aliceEvents := alice.RPC.SubscribeHtlcEvents()
	bobEvents := bob.RPC.SubscribeHtlcEvents()
	carolEvents := carol.RPC.SubscribeHtlcEvents()

	// Once subscribed, the first event will be UNKNOWN.
	ht.AssertHtlcEventType(aliceEvents, routerrpc.HtlcEvent_UNKNOWN)
	ht.AssertHtlcEventType(bobEvents, routerrpc.HtlcEvent_UNKNOWN)
	ht.AssertHtlcEventType(carolEvents, routerrpc.HtlcEvent_UNKNOWN)

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob.
	chanPointAlice := ht.OpenChannel(
		alice, bob,
		lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Next, we'll create a connection from Bob to Carol, and open a
	// channel between them so we have the topology: Alice -> Bob -> Carol.
	// The channel created will be of lower capacity that the one created
	// above.
	chanPointBob := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Ensure that Alice has Carol in her routing table before proceeding.
	ht.AssertChannelInGraph(alice, chanPointBob)

	cType := ht.GetChannelCommitType(alice, chanPointAlice)
	commitFee := lntest.CalcStaticFee(cType, 0)

	assertBaseBalance := func() {
		// Alice has opened a channel with Bob with zero push amount,
		// so it's remote balance is zero.
		expBalanceAlice := &lnrpc.ChannelBalanceResponse{
			LocalBalance: &lnrpc.Amount{
				Sat: uint64(chanAmt - commitFee),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					chanAmt - commitFee,
				)),
			},
			RemoteBalance:            &lnrpc.Amount{},
			UnsettledLocalBalance:    &lnrpc.Amount{},
			UnsettledRemoteBalance:   &lnrpc.Amount{},
			PendingOpenLocalBalance:  &lnrpc.Amount{},
			PendingOpenRemoteBalance: &lnrpc.Amount{},
			// Deprecated fields.
			Balance: int64(chanAmt - commitFee),
		}
		ht.AssertChannelBalanceResp(alice, expBalanceAlice)

		// Bob has a channel with Alice and another with Carol, so it's
		// local and remote balances are both chanAmt - commitFee.
		expBalanceBob := &lnrpc.ChannelBalanceResponse{
			LocalBalance: &lnrpc.Amount{
				Sat: uint64(chanAmt - commitFee),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					chanAmt - commitFee,
				)),
			},
			RemoteBalance: &lnrpc.Amount{
				Sat: uint64(chanAmt - commitFee),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					chanAmt - commitFee,
				)),
			},
			UnsettledLocalBalance:    &lnrpc.Amount{},
			UnsettledRemoteBalance:   &lnrpc.Amount{},
			PendingOpenLocalBalance:  &lnrpc.Amount{},
			PendingOpenRemoteBalance: &lnrpc.Amount{},
			// Deprecated fields.
			Balance: int64(chanAmt - commitFee),
		}
		ht.AssertChannelBalanceResp(bob, expBalanceBob)
	}

	// assertLinkFailure checks that the stream provided has a single link
	// failure the failure detail provided.
	assertLinkFailure := func(event *routerrpc.HtlcEvent,
		failureDetail routerrpc.FailureDetail) {

		linkFail, ok := event.Event.(*routerrpc.HtlcEvent_LinkFailEvent)
		require.Truef(ht, ok, "expected forwarding failure, got: %T",
			linkFail)

		require.Equal(ht, failureDetail,
			linkFail.LinkFailEvent.FailureDetail,
			"wrong link fail detail")
	}

	// With the channels, open we can now start to test our multi-hop error
	// scenarios. First, we'll generate an invoice from carol that we'll
	// use to test some error cases.
	const payAmt = 10000
	invoiceReq := &lnrpc.Invoice{
		Memo:  "kek99",
		Value: payAmt,
	}

	carolInvoice := carol.RPC.AddInvoice(invoiceReq)
	carolPayReq := carol.RPC.DecodePayReq(carolInvoice.PaymentRequest)

	// For the first scenario, we'll test the cancellation of an HTLC with
	// an unknown payment hash.
	sendReq := &routerrpc.SendPaymentRequest{
		PaymentHash:    ht.Random32Bytes(),
		Dest:           carol.PubKey[:],
		Amt:            payAmt,
		FinalCltvDelta: int32(carolPayReq.CltvExpiry),
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}
	ht.SendPaymentAssertFail(
		alice, sendReq,
		lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS, //nolint:ll
	)
	ht.AssertLastHTLCError(
		alice, lnrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS,
	)

	// assertAliceAndBob is a helper closure that asserts Alice and Bob
	// each has one forward and one forward fail event, and Bob has the
	// final htlc fail event.
	assertAliceAndBob := func() {
		ht.AssertHtlcEventTypes(
			aliceEvents, routerrpc.HtlcEvent_SEND,
			lntest.HtlcEventForward,
		)
		ht.AssertHtlcEventTypes(
			aliceEvents, routerrpc.HtlcEvent_SEND,
			lntest.HtlcEventForwardFail,
		)

		ht.AssertHtlcEventTypes(
			bobEvents, routerrpc.HtlcEvent_FORWARD,
			lntest.HtlcEventForward,
		)
		ht.AssertHtlcEventTypes(
			bobEvents, routerrpc.HtlcEvent_FORWARD,
			lntest.HtlcEventForwardFail,
		)
		ht.AssertHtlcEventTypes(
			bobEvents, routerrpc.HtlcEvent_UNKNOWN,
			lntest.HtlcEventFinal,
		)
	}

	// We expect alice and bob to each have one forward and one forward
	// fail event at this stage.
	assertAliceAndBob()

	// Carol should have a link failure because the htlc failed on her
	// incoming link.
	event := ht.AssertHtlcEventType(
		carolEvents, routerrpc.HtlcEvent_RECEIVE,
	)
	assertLinkFailure(event, routerrpc.FailureDetail_UNKNOWN_INVOICE)

	// There's also a final htlc event that gives the final outcome of the
	// htlc.
	ht.AssertHtlcEventTypes(
		carolEvents, routerrpc.HtlcEvent_UNKNOWN, lntest.HtlcEventFinal,
	)

	// The balances of all parties should be the same as initially since
	// the HTLC was canceled.
	assertBaseBalance()

	// Next, we'll test the case of a recognized payHash but, an incorrect
	// value on the extended HTLC.
	htlcAmt := lnwire.NewMSatFromSatoshis(1000)
	sendReq = &routerrpc.SendPaymentRequest{
		PaymentHash: carolInvoice.RHash,
		Dest:        carol.PubKey[:],
		// 10k satoshis are expected.
		Amt:            int64(htlcAmt.ToSatoshis()),
		FinalCltvDelta: int32(carolPayReq.CltvExpiry),
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}
	ht.SendPaymentAssertFail(
		alice, sendReq,
		lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS, //nolint:ll
	)
	ht.AssertLastHTLCError(
		alice, lnrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS,
	)

	// We expect alice and bob to each have one forward and one forward
	// fail event at this stage.
	assertAliceAndBob()

	// Carol should have a link failure because the htlc failed on her
	// incoming link.
	event = ht.AssertHtlcEventType(carolEvents, routerrpc.HtlcEvent_RECEIVE)
	assertLinkFailure(event, routerrpc.FailureDetail_INVOICE_UNDERPAID)

	// There's also a final htlc event that gives the final outcome of the
	// htlc.
	ht.AssertHtlcEventTypes(
		carolEvents, routerrpc.HtlcEvent_UNKNOWN, lntest.HtlcEventFinal,
	)

	// The balances of all parties should be the same as initially since
	// the HTLC was canceled.
	assertBaseBalance()

	// Next we'll test an error that occurs mid-route due to an outgoing
	// link having insufficient capacity. In order to do so, we'll first
	// need to unbalance the link connecting Bob<->Carol.
	//
	// To do so, we'll push most of the funds in the channel over to
	// Alice's side, leaving on 10k satoshis of available balance for bob.
	chanReserve := int64(chanAmt / 100)
	feeBuffer := lntest.CalcStaticFeeBuffer(cType, 0)
	amtToSend := int64(chanAmt) - chanReserve - int64(feeBuffer) - 10000

	invoiceReq = &lnrpc.Invoice{
		Value: amtToSend,
	}
	carolInvoice2 := carol.RPC.AddInvoice(invoiceReq)

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice2.PaymentRequest,
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}
	ht.SendPaymentAndAssertStatus(bob, req, lnrpc.Payment_SUCCEEDED)

	// We need to check that bob has a forward and settle event for his
	// send, and carol has a settle event and a final htlc event for her
	// receive.
	ht.AssertHtlcEventTypes(
		bobEvents, routerrpc.HtlcEvent_SEND,
		lntest.HtlcEventForward,
	)
	ht.AssertHtlcEventTypes(
		bobEvents, routerrpc.HtlcEvent_SEND,
		lntest.HtlcEventSettle,
	)
	ht.AssertHtlcEventTypes(
		carolEvents, routerrpc.HtlcEvent_RECEIVE,
		lntest.HtlcEventSettle,
	)
	ht.AssertHtlcEventTypes(
		carolEvents, routerrpc.HtlcEvent_UNKNOWN,
		lntest.HtlcEventFinal,
	)

	// At this point, Alice has 50mil satoshis on her side of the channel,
	// but Bob only has 10k available on his side of the channel. So a
	// payment from Alice to Carol worth 100k satoshis should fail.
	invoiceReq = &lnrpc.Invoice{
		Value: 100000,
	}
	carolInvoice3 := carol.RPC.AddInvoice(invoiceReq)

	sendReq = &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice3.PaymentRequest,
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}
	ht.SendPaymentAssertFail(
		alice, sendReq,
		lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE,
	)
	ht.AssertLastHTLCError(
		alice, lnrpc.Failure_TEMPORARY_CHANNEL_FAILURE,
	)

	// Alice should have a forwarding event and a forwarding failure.
	ht.AssertHtlcEventTypes(
		aliceEvents, routerrpc.HtlcEvent_SEND,
		lntest.HtlcEventForward,
	)
	ht.AssertHtlcEventTypes(
		aliceEvents, routerrpc.HtlcEvent_SEND,
		lntest.HtlcEventForwardFail,
	)

	// Bob should have a link failure because the htlc failed on his
	// outgoing link.
	event = ht.AssertHtlcEventType(bobEvents, routerrpc.HtlcEvent_FORWARD)
	assertLinkFailure(event, routerrpc.FailureDetail_INSUFFICIENT_BALANCE)

	// There's also a final htlc event that gives the final outcome of the
	// htlc.
	ht.AssertHtlcEventTypes(
		bobEvents, routerrpc.HtlcEvent_UNKNOWN, lntest.HtlcEventFinal,
	)

	// Generate new invoice to not pay same invoice twice.
	carolInvoice = carol.RPC.AddInvoice(invoiceReq)

	// For our final test, we'll ensure that if a target link isn't
	// available for what ever reason then the payment fails accordingly.
	//
	// We'll attempt to complete the original invoice we created with Carol
	// above, but before we do so, Carol will go offline, resulting in a
	// failed payment.
	ht.Shutdown(carol)

	// Reset mission control to forget the temporary channel failure above.
	alice.RPC.ResetMissionControl()

	req = &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}
	ht.SendPaymentAssertFail(
		alice, req, lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE,
	)
	ht.AssertLastHTLCError(alice, lnrpc.Failure_UNKNOWN_NEXT_PEER)

	// Alice should have a forwarding event and subsequent fail.
	ht.AssertHtlcEventTypes(
		aliceEvents, routerrpc.HtlcEvent_SEND,
		lntest.HtlcEventForward,
	)
	ht.AssertHtlcEventTypes(
		aliceEvents, routerrpc.HtlcEvent_SEND,
		lntest.HtlcEventForwardFail,
	)

	// Bob should have a link failure because he could not find the next
	// peer.
	event = ht.AssertHtlcEventType(bobEvents, routerrpc.HtlcEvent_FORWARD)
	assertLinkFailure(event, routerrpc.FailureDetail_NO_DETAIL)

	// There's also a final htlc event that gives the final outcome of the
	// htlc.
	ht.AssertHtlcEventTypes(
		bobEvents, routerrpc.HtlcEvent_UNKNOWN, lntest.HtlcEventFinal,
	)
}
