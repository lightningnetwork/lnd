package itest

import (
	"math"

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

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob.
	alice, bob := ht.Alice(), ht.Bob()
	chanPointAlice := ht.OpenChannel(
		alice, bob,
		lntest.OpenChannelParams{Amt: chanAmt},
	)

	cType := ht.GetChannelCommitType(alice, chanPointAlice)

	commitFee := calcStaticFee(cType, 0)
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
	// failure the the failure detail provided.
	assertLinkFailure := func(event *routerrpc.HtlcEvent,
		failureDetail routerrpc.FailureDetail) {

		linkFail, ok := event.Event.(*routerrpc.HtlcEvent_LinkFailEvent)
		require.Truef(ht, ok, "expected forwarding failure, got: %T",
			linkFail)

		require.Equal(ht, failureDetail,
			linkFail.LinkFailEvent.FailureDetail,
			"wrong link fail detail")
	}

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol := ht.NewNode("Carol", nil)

	// Next, we'll create a connection from Bob to Carol, and open a
	// channel between them so we have the topology: Alice -> Bob -> Carol.
	// The channel created will be of lower capacity that the one created
	// above.
	ht.ConnectNodes(bob, carol)
	const bobChanAmt = funding.MaxBtcFundingAmount
	chanPointBob := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Ensure that Alice has Carol in her routing table before proceeding.
	ht.AssertChannelOpen(alice, chanPointBob)

	// With the channels, open we can now start to test our multi-hop error
	// scenarios. First, we'll generate an invoice from carol that we'll
	// use to test some error cases.
	const payAmt = 10000
	invoiceReq := &lnrpc.Invoice{
		Memo:  "kek99",
		Value: payAmt,
	}
	carolInvoice := ht.AddInvoice(invoiceReq, carol)
	carolPayReq := ht.DecodePayReq(carol, carolInvoice.PaymentRequest)

	// Before we start sending payments, subscribe to htlc events for each
	// node.
	aliceEvents := ht.SubscribeHtlcEvents(alice)
	bobEvents := ht.SubscribeHtlcEvents(bob)
	carolEvents := ht.SubscribeHtlcEvents(carol)

	// For the first scenario, we'll test the cancellation of an HTLC with
	// an unknown payment hash.
	sendReq := &routerrpc.SendPaymentRequest{
		PaymentHash:    ht.Random32Bytes(),
		Dest:           carol.PubKey[:],
		Amt:            payAmt,
		FinalCltvDelta: int32(carolPayReq.CltvExpiry),
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}
	ht.SendPaymentAssertFail(
		alice, sendReq,
		lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS,
	)
	ht.AssertLastHTLCError(
		alice, lnrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS,
	)

	// We expect alice and bob to each have one forward and one forward
	// fail event at this stage.
	ht.AssertHtlcEvents(aliceEvents, 1, 1, 0, 0, routerrpc.HtlcEvent_SEND)
	ht.AssertHtlcEvents(bobEvents, 1, 1, 0, 0, routerrpc.HtlcEvent_FORWARD)

	// Carol should have a link failure because the htlc failed on her
	// incoming link.
	event := ht.AssertHtlcEvents(
		carolEvents, 0, 0, 0, 1, routerrpc.HtlcEvent_RECEIVE,
	)[0]
	assertLinkFailure(event, routerrpc.FailureDetail_UNKNOWN_INVOICE)

	// The balances of all parties should be the same as initially since
	// the HTLC was canceled.
	assertBaseBalance()

	// Next, we'll test the case of a recognized payHash but, an incorrect
	// value on the extended HTLC.
	htlcAmt := lnwire.NewMSatFromSatoshis(1000)
	sendReq = &routerrpc.SendPaymentRequest{
		PaymentHash:    carolInvoice.RHash,
		Dest:           carol.PubKey[:],
		Amt:            int64(htlcAmt.ToSatoshis()), // 10k satoshis are expected.
		FinalCltvDelta: int32(carolPayReq.CltvExpiry),
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}
	ht.SendPaymentAssertFail(
		alice, sendReq,
		lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS,
	)
	ht.AssertLastHTLCError(
		alice, lnrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS,
	)

	// We expect alice and bob to each have one forward and one forward
	// fail event at this stage.
	ht.AssertHtlcEvents(aliceEvents, 1, 1, 0, 0, routerrpc.HtlcEvent_SEND)
	ht.AssertHtlcEvents(bobEvents, 1, 1, 0, 0, routerrpc.HtlcEvent_FORWARD)

	// Carol should have a link failure because the htlc failed on her
	// incoming link.
	event = ht.AssertHtlcEvents(
		carolEvents, 0, 0, 0, 1, routerrpc.HtlcEvent_RECEIVE,
	)[0]
	assertLinkFailure(event, routerrpc.FailureDetail_INVOICE_UNDERPAID)

	// The balances of all parties should be the same as initially since
	// the HTLC was canceled.
	assertBaseBalance()

	// Next we'll test an error that occurs mid-route due to an outgoing
	// link having insufficient capacity. In order to do so, we'll first
	// need to unbalance the link connecting Bob<->Carol.
	//
	// To do so, we'll push most of the funds in the channel over to
	// Alice's side, leaving on 10k satoshis of available balance for bob.
	// There's a max payment amount, so we'll have to do this
	// incrementally.
	chanReserve := int64(chanAmt / 100)
	amtToSend := int64(chanAmt) - chanReserve - 20000
	amtSent := int64(0)
	for amtSent != amtToSend {
		// We'll send in chunks of the max payment amount. If we're
		// about to send too much, then we'll only send the amount
		// remaining.
		toSend := int64(math.MaxUint32)
		if toSend+amtSent > amtToSend {
			toSend = amtToSend - amtSent
		}

		invoiceReq = &lnrpc.Invoice{
			Value: toSend,
		}
		carolInvoice2 := ht.AddInvoice(invoiceReq, carol)

		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: carolInvoice2.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
			MaxParts:       1,
		}
		ht.SendPaymentAndAssert(bob, req)

		// For each send bob makes, we need to check that bob has a
		// forward and settle event for his send, and carol has a
		// settle event for her receive.
		ht.AssertHtlcEvents(
			bobEvents, 1, 0, 1, 0, routerrpc.HtlcEvent_SEND,
		)
		ht.AssertHtlcEvents(
			carolEvents, 0, 0, 1, 0, routerrpc.HtlcEvent_RECEIVE,
		)

		amtSent += toSend
	}

	// At this point, Alice has 50mil satoshis on her side of the channel,
	// but Bob only has 10k available on his side of the channel. So a
	// payment from Alice to Carol worth 100k satoshis should fail.
	invoiceReq = &lnrpc.Invoice{
		Value: 100000,
	}
	carolInvoice3 := ht.AddInvoice(invoiceReq, carol)

	sendReq = &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice3.PaymentRequest,
		TimeoutSeconds: 60,
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
	ht.AssertHtlcEvents(aliceEvents, 1, 1, 0, 0, routerrpc.HtlcEvent_SEND)

	// Bob should have a link failure because the htlc failed on his
	// outgoing link.
	event = ht.AssertHtlcEvents(
		bobEvents, 0, 0, 0, 1, routerrpc.HtlcEvent_FORWARD,
	)[0]
	assertLinkFailure(event, routerrpc.FailureDetail_INSUFFICIENT_BALANCE)

	// Generate new invoice to not pay same invoice twice.
	carolInvoice = ht.AddInvoice(invoiceReq, carol)

	// For our final test, we'll ensure that if a target link isn't
	// available for what ever reason then the payment fails accordingly.
	//
	// We'll attempt to complete the original invoice we created with Carol
	// above, but before we do so, Carol will go offline, resulting in a
	// failed payment.
	ht.Shutdown(carol)

	// Reset mission control to forget the temporary channel failure above.
	ht.ResetMissionControl(alice)

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		MaxParts:       1,
	}
	ht.SendPaymentAssertFail(
		alice, req, lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE,
	)
	ht.AssertLastHTLCError(alice, lnrpc.Failure_UNKNOWN_NEXT_PEER)

	// Alice should have a forwarding event and subsequent fail.
	ht.AssertHtlcEvents(aliceEvents, 1, 1, 0, 0, routerrpc.HtlcEvent_SEND)

	// Bob should have a link failure because he could not find the next
	// peer.
	event = ht.AssertHtlcEvents(
		bobEvents, 0, 0, 0, 1, routerrpc.HtlcEvent_FORWARD,
	)[0]
	assertLinkFailure(event, routerrpc.FailureDetail_NO_DETAIL)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPointAlice, false)

	// Force close Bob's final channel.
	ht.CloseChannel(bob, chanPointBob, true)

	// Cleanup by mining the force close and sweep transaction.
	ht.CleanupForceClose(bob, chanPointBob)
}
