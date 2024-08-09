package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
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
	cpAB, cpBC := resp[0], resp[1]

	// Make sure Alice is aware of channel Bob=>Carol.
	ht.AssertTopologyChannelOpen(alice, cpBC)

	// Initiate Carol's invoice HTLC modifier.
	invoiceModifier, cancelModifier := carol.RPC.InvoiceHtlcModifier()

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

		// For all other packets we resolve according to the test case.
		err := invoiceModifier.Send(
			&invoicesrpc.HtlcModifyResponse{
				CircuitKey: modifierRequest.ExitHtlcCircuitKey,
				AmtPaid:    uint64(tc.invoiceAmountMsat),
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

		ht.Log("Ensure invoice status is settled")
		require.Eventually(ht, func() bool {
			updatedInvoice := carol.RPC.LookupInvoice(
				tc.invoice.RHash,
			)

			return updatedInvoice.State == tc.finalInvoiceState
		}, defaultTimeout, 1*time.Second)
	}

	cancelModifier()

	// Finally, close channels.
	ht.CloseChannel(alice, cpAB)
	ht.CloseChannel(bob, cpBC)
}

// acceptorTestCase is a helper struct to hold test case data.
type acceptorTestCase struct {
	// invoiceAmountMsat is the amount of the invoice.
	invoiceAmountMsat int64

	// sendAmountMsat is the amount that will be sent in the payment.
	sendAmountMsat int64

	// skipAmtCheck is a flag that indicates whether the amount checks
	// should be skipped during the invoice settlement process.
	skipAmtCheck bool

	// finalInvoiceState is the expected eventual final state of the
	// invoice.
	finalInvoiceState lnrpc.Invoice_InvoiceState

	// payAddr is the payment address of the invoice.
	payAddr []byte

	// invoice is the invoice that will be paid.
	invoice *lnrpc.Invoice
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
func newAcceptorTestScenario(
	ht *lntest.HarnessTest) *acceptorTestScenario {

	alice, bob := ht.Alice, ht.Bob
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
			skipAmtCheck:      true,
			finalInvoiceState: lnrpc.Invoice_SETTLED,
		},
	}

	for _, t := range cases {
		inv := &lnrpc.Invoice{ValueMsat: t.invoiceAmountMsat}
		addResponse := c.carol.RPC.AddInvoice(inv)
		invoice := c.carol.RPC.LookupInvoice(addResponse.RHash)

		// We'll need to also decode the returned invoice so we can
		// grab the payment address which is now required for ALL
		// payments.
		payReq := c.carol.RPC.DecodePayReq(invoice.PaymentRequest)

		t.invoice = invoice
		t.payAddr = payReq.PaymentAddr
	}

	return cases
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
