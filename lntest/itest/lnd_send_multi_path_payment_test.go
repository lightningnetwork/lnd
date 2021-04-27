package itest

import (
	"context"
	"encoding/hex"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

// testSendMultiPathPayment tests that we are able to successfully route a
// payment using multiple shards across different paths.
func testSendMultiPathPayment(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	ctx := newMppTestContext(t, net)
	defer ctx.shutdownNodes()

	const paymentAmt = btcutil.Amount(300000)

	// Set up a network with three different paths Alice <-> Bob. Channel
	// capacities are set such that the payment can only succeed if (at
	// least) three paths are used.
	//
	//              _ Eve _
	//             /       \
	// Alice -- Carol ---- Bob
	//      \              /
	//       \__ Dave ____/
	//
	ctx.openChannel(ctx.carol, ctx.bob, 135000)
	ctx.openChannel(ctx.alice, ctx.carol, 235000)
	ctx.openChannel(ctx.dave, ctx.bob, 135000)
	ctx.openChannel(ctx.alice, ctx.dave, 135000)
	ctx.openChannel(ctx.eve, ctx.bob, 135000)
	ctx.openChannel(ctx.carol, ctx.eve, 135000)

	defer ctx.closeChannels()

	ctx.waitForChannels()

	// Increase Dave's fee to make the test deterministic. Otherwise it
	// would be unpredictable whether pathfinding would go through Charlie
	// or Dave for the first shard.
	_, err := ctx.dave.UpdateChannelPolicy(
		context.Background(),
		&lnrpc.PolicyUpdateRequest{
			Scope:         &lnrpc.PolicyUpdateRequest_Global{Global: true},
			BaseFeeMsat:   500000,
			FeeRate:       0.001,
			TimeLockDelta: 40,
		},
	)
	if err != nil {
		t.Fatalf("dave policy update: %v", err)
	}
	// Our first test will be Alice paying Bob using a SendPayment call.
	// Let Bob create an invoice for Alice to pay.
	payReqs, rHashes, invoices, err := createPayReqs(
		net.Bob, paymentAmt, 1,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	rHash := rHashes[0]
	payReq := payReqs[0]

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	payment := sendAndAssertSuccess(
		ctxt, t, net.Alice,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			MaxParts:       10,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// Make sure we got the preimage.
	if payment.PaymentPreimage != hex.EncodeToString(invoices[0].RPreimage) {
		t.Fatalf("preimage doesn't match")
	}

	// Check that Alice split the payment in at least three shards. Because
	// the hand-off of the htlc to the link is asynchronous (via a mailbox),
	// there is some non-determinism in the process. Depending on whether
	// the new pathfinding round is started before or after the htlc is
	// locked into the channel, different sharding may occur. Therefore we
	// can only check if the number of shards isn't below the theoretical
	// minimum.
	succeeded := 0
	for _, htlc := range payment.Htlcs {
		if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
			succeeded++
		}
	}

	const minExpectedShards = 3
	if succeeded < minExpectedShards {
		t.Fatalf("expected at least %v shards, but got %v",
			minExpectedShards, succeeded)
	}

	// Make sure Bob show the invoice as settled for the full
	// amount.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	inv, err := ctx.bob.LookupInvoice(
		ctxt, &lnrpc.PaymentHash{
			RHash: rHash,
		},
	)
	if err != nil {
		t.Fatalf("error when obtaining invoice: %v", err)
	}

	if inv.AmtPaidSat != int64(paymentAmt) {
		t.Fatalf("incorrect payment amt for invoice"+
			"want: %d, got %d",
			paymentAmt, inv.AmtPaidSat)
	}

	if inv.State != lnrpc.Invoice_SETTLED {
		t.Fatalf("Invoice not settled: %v", inv.State)
	}

	settled := 0
	for _, htlc := range inv.Htlcs {
		if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
			settled++
		}

	}
	if settled != succeeded {
		t.Fatalf("expected invoice to be settled "+
			"with %v HTLCs, had %v", succeeded, settled)
	}
}
