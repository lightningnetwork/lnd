package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// testInvoiceHtlcModifierBasic tests the basic functionality of the invoice
// HTLC modifier RPC server.
func testInvoiceHtlcModifierBasic(ht *lntest.HarnessTest) {
	ts := newAcceptorTestScenario(ht)

	alice, bob, carol := ts.alice, ts.bob, ts.carol

	// Open and wait for channels.
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: p},
		{Local: bob, Remote: carol, Param: p},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)
	cpBC := resp[1]

	// Make sure Alice is aware of channel Bob=>Carol.
	ht.AssertChannelInGraph(alice, cpBC)

	// Initiate Carol's invoice HTLC modifier.
	invoiceModifier, cancelModifier := carol.RPC.InvoiceHtlcModifier()

	// We need to wait a bit to make sure the gRPC stream is established
	// correctly.
	time.Sleep(50 * time.Millisecond)

	// Make sure we get an error if we try to register a second modifier and
	// then try to use it (the error won't be returned on connect, only on
	// the first _read_ interaction on the stream).
	mod2, err := carol.RPC.Invoice.HtlcModifier(ht.Context())
	require.NoError(ht, err)
	_, err = mod2.Recv()
	require.ErrorContains(
		ht, err,
		invoices.ErrInterceptorClientAlreadyConnected.Error(),
	)

	// We also add a normal (forwarding) HTLC interceptor at Bob, so we can
	// test that custom wire messages on the HTLC are forwarded correctly to
	// the invoice HTLC interceptor.
	bobInterceptor, bobInterceptorCancel := bob.RPC.HtlcInterceptor()

	// Prepare the test cases.
	testCases := ts.prepareTestCases()

	for tcIdx, tc := range testCases {
		ht.Logf("Running test case: %d", tcIdx)

		// Initiate a payment from Alice to Carol in a separate
		// goroutine. We use a separate goroutine to avoid blocking the
		// main goroutine where we will make use of the invoice
		// acceptor.
		sendPaymentDone := make(chan struct{})
		go func() {
			// Signal that all the payments have been sent.
			defer close(sendPaymentDone)

			_ = ts.sendPayment(tc)
		}()

		// First, intercept the HTLC at Bob.
		packet := ht.ReceiveHtlcInterceptor(bobInterceptor)
		err := bobInterceptor.Send(
			&routerrpc.ForwardHtlcInterceptResponse{
				IncomingCircuitKey:   packet.IncomingCircuitKey,
				OutAmountMsat:        packet.OutgoingAmountMsat,
				OutWireCustomRecords: tc.lastHopCustomRecords,
				Action:               actionResumeModify,
			},
		)
		require.NoError(ht, err, "failed to send request")

		modifierRequest := ht.ReceiveInvoiceHtlcModification(
			invoiceModifier,
		)

		// Sanity check the modifier request.
		require.EqualValues(
			ht, tc.invoiceAmountMsat,
			modifierRequest.Invoice.ValueMsat,
		)
		require.EqualValues(
			ht, tc.sendAmountMsat, modifierRequest.ExitHtlcAmt,
		)

		// Expect custom records plus endorsement signal.
		require.Equal(
			ht, lntest.CustomRecordsWithUnendorsed(
				tc.lastHopCustomRecords,
			), modifierRequest.ExitHtlcWireCustomRecords,
		)

		// For all other packets we resolve according to the test case.
		amtPaid := uint64(tc.invoiceAmountMsat)
		err = invoiceModifier.Send(
			&invoicesrpc.HtlcModifyResponse{
				CircuitKey: modifierRequest.ExitHtlcCircuitKey,
				AmtPaid:    &amtPaid,
				CancelSet:  tc.cancelSet,
			},
		)
		require.NoError(ht, err, "failed to send request")

		ht.Log("Waiting for payment send to complete")
		select {
		case <-sendPaymentDone:
			ht.Log("Payment send attempt complete")
		case <-time.After(defaultTimeout):
			require.Fail(ht, "timeout waiting for payment send")
		}

		ht.Logf("Ensure invoice status is expected state %v",
			tc.finalInvoiceState)

		require.Eventually(ht, func() bool {
			updatedInvoice := carol.RPC.LookupInvoice(
				tc.invoice.RHash,
			)

			return updatedInvoice.State == tc.finalInvoiceState
		}, defaultTimeout, 1*time.Second)

		updatedInvoice := carol.RPC.LookupInvoice(
			tc.invoice.RHash,
		)

		// If the HTLC modifier canceled the incoming HTLC set, we don't
		// expect any HTLCs in the invoice.
		if tc.cancelSet {
			require.Len(ht, updatedInvoice.Htlcs, 0)
			return
		}

		require.Len(ht, updatedInvoice.Htlcs, 1)
		require.Equal(
			ht, lntest.CustomRecordsWithUnendorsed(
				tc.lastHopCustomRecords,
			), updatedInvoice.Htlcs[0].CustomRecords,
		)

		// Make sure the custom channel data contains the encoded
		// version of the custom records.
		customRecords := lnwire.CustomRecords(
			updatedInvoice.Htlcs[0].CustomRecords,
		)
		encodedRecords, err := customRecords.Serialize()
		require.NoError(ht, err)

		require.Equal(
			ht, encodedRecords,
			updatedInvoice.Htlcs[0].CustomChannelData,
		)
	}

	// We don't need the HTLC interceptor at Bob anymore.
	bobInterceptorCancel()

	// After the normal test cases, we test that we can shut down Carol
	// while an HTLC interception is going on. We initiate a payment from
	// Alice to Carol in a separate goroutine. We use a separate goroutine
	// to avoid blocking the main goroutine where we will make use of the
	// invoice acceptor.
	sendPaymentDone := make(chan struct{})
	tc := &acceptorTestCase{
		invoiceAmountMsat: 9000,
		sendAmountMsat:    9000,
	}
	ts.createInvoice(tc)

	go func() {
		// Signal that all the payments have been sent.
		defer close(sendPaymentDone)

		_ = ts.sendPayment(tc)
	}()

	modifierRequest := ht.ReceiveInvoiceHtlcModification(invoiceModifier)

	// Sanity check the modifier request.
	require.EqualValues(
		ht, tc.invoiceAmountMsat, modifierRequest.Invoice.ValueMsat,
	)
	require.EqualValues(
		ht, tc.sendAmountMsat, modifierRequest.ExitHtlcAmt,
	)

	// We don't send a response to the modifier, but instead shut down and
	// restart Carol.
	restart := ht.SuspendNode(carol)
	require.NoError(ht, restart())

	ht.Log("Waiting for payment send to complete")
	select {
	case <-sendPaymentDone:
		ht.Log("Payment send attempt complete")
	case <-time.After(defaultTimeout):
		require.Fail(ht, "timeout waiting for payment send")
	}

	cancelModifier()
}

// acceptorTestCase is a helper struct to hold test case data.
type acceptorTestCase struct {
	// invoiceAmountMsat is the amount of the invoice.
	invoiceAmountMsat int64

	// sendAmountMsat is the amount that will be sent in the payment.
	sendAmountMsat int64

	// lastHopCustomRecords is a map of custom records that will be added
	// to the last hop of the payment, by an HTLC interceptor at Bob.
	lastHopCustomRecords map[uint64][]byte

	// finalInvoiceState is the expected eventual final state of the
	// invoice.
	finalInvoiceState lnrpc.Invoice_InvoiceState

	// payAddr is the payment address of the invoice.
	payAddr []byte

	// invoice is the invoice that will be paid.
	invoice *lnrpc.Invoice

	// cancelSet is a boolean which indicates whether the HTLC modifier
	// canceled the incoming HTLC set.
	cancelSet bool
}

// acceptorTestScenario is a helper struct to hold the test context and provides
// helpful functionality.
type acceptorTestScenario struct {
	ht                *lntest.HarnessTest
	alice, bob, carol *node.HarnessNode
}

// newAcceptorTestScenario initializes a new test scenario with three nodes and
// connects them to have the following topology,
//
//	Alice --> Bob --> Carol
//
// Among them, Alice and Bob are standby nodes and Carol is a new node.
func newAcceptorTestScenario(ht *lntest.HarnessTest) *acceptorTestScenario {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("bob", nil)
	carol := ht.NewNode("carol", nil)

	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)

	return &acceptorTestScenario{
		ht:    ht,
		alice: alice,
		bob:   bob,
		carol: carol,
	}
}

// prepareTestCases prepares test cases.
func (c *acceptorTestScenario) prepareTestCases() []*acceptorTestCase {
	cases := []*acceptorTestCase{
		// Send a payment with amount less than the invoice amount.
		// Amount checking is skipped during the invoice settlement
		// process. The sent payment should eventually result in the
		// invoice being settled.
		{
			invoiceAmountMsat: 9000,
			sendAmountMsat:    1000,
			finalInvoiceState: lnrpc.Invoice_SETTLED,
		},
		{
			invoiceAmountMsat: 9000,
			sendAmountMsat:    1000,
			finalInvoiceState: lnrpc.Invoice_SETTLED,
			lastHopCustomRecords: map[uint64][]byte{
				lnwire.MinCustomRecordsTlvType: {1, 2, 3},
			},
		},
		{
			invoiceAmountMsat: 9000,
			sendAmountMsat:    1000,
			finalInvoiceState: lnrpc.Invoice_OPEN,
			cancelSet:         true,
		},
	}

	for _, t := range cases {
		c.createInvoice(t)
	}

	return cases
}

// createInvoice creates an invoice for the given test case.
func (c *acceptorTestScenario) createInvoice(tc *acceptorTestCase) {
	inv := &lnrpc.Invoice{ValueMsat: tc.invoiceAmountMsat}
	addResponse := c.carol.RPC.AddInvoice(inv)
	invoice := c.carol.RPC.LookupInvoice(addResponse.RHash)

	// We'll need to also decode the returned invoice so we can grab the
	// payment address which is now required for ALL payments.
	payReq := c.carol.RPC.DecodePayReq(invoice.PaymentRequest)

	tc.invoice = invoice
	tc.payAddr = payReq.PaymentAddr
}

// buildRoute is a helper function to build a route with given hops.
func (c *acceptorTestScenario) buildRoute(amtMsat int64,
	hops []*node.HarnessNode, payAddr []byte) *lnrpc.Route {

	rpcHops := make([][]byte, 0, len(hops))
	for _, hop := range hops {
		k := hop.PubKeyStr
		pubkey, err := route.NewVertexFromStr(k)
		require.NoErrorf(c.ht, err, "error parsing %v: %v", k, err)
		rpcHops = append(rpcHops, pubkey[:])
	}

	req := &routerrpc.BuildRouteRequest{
		AmtMsat:        amtMsat,
		FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
		HopPubkeys:     rpcHops,
		PaymentAddr:    payAddr,
	}

	routeResp := c.alice.RPC.BuildRoute(req)

	return routeResp.Route
}

// sendPaymentAndAssertAction sends a payment from alice to carol.
func (c *acceptorTestScenario) sendPayment(
	tc *acceptorTestCase) *lnrpc.HTLCAttempt {

	// Build a route from alice to carol.
	aliceBobCarolRoute := c.buildRoute(
		tc.sendAmountMsat, []*node.HarnessNode{c.bob, c.carol},
		tc.payAddr,
	)

	// We need to cheat a bit. We are attempting to pay an invoice with
	// amount X with an HTLC of amount Y that is less than X. And then we
	// use the invoice HTLC interceptor to simulate the HTLC actually
	// carrying amount X (even though the actual HTLC transaction output
	// only has amount Y). But in order for the invoice to be settled, we
	// need to make sure that the MPP total amount record in the last hop
	// is set to the invoice amount. This would also be the case in a normal
	// MPP payment, where each shard only pays a fraction of the invoice.
	aliceBobCarolRoute.Hops[1].MppRecord.TotalAmtMsat = tc.invoiceAmountMsat

	// Send the payment.
	sendReq := &routerrpc.SendToRouteRequest{
		PaymentHash: tc.invoice.RHash,
		Route:       aliceBobCarolRoute,
	}

	return c.alice.RPC.SendToRouteV2(sendReq)
}
