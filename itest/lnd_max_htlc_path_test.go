package itest

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

// testMaxHtlcPathPayment tests that when a payment is attempted, the path
// finding logic correctly takes into account the max_htlc value of the first
// channel.
func testMaxHtlcPathPayment(ht *lntest.HarnessTest) {
	// Create a channel Alice->Bob.
	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil}, lntest.OpenChannelParams{
			Amt: 1000000,
		},
	)
	alice, bob := nodes[0], nodes[1]
	chanPoint := chanPoints[0]

	// Alice and Bob should have one channel open with each other now.
	ht.AssertNodeNumChannels(alice, 1)
	ht.AssertNodeNumChannels(bob, 1)

	// Define a max_htlc value that is lower than the default.
	const (
		maxHtlcMsat   = 50000000
		minHtlcMsat   = 1000
		timeLockDelta = 80
		baseFeeMsat   = 0
		feeRate       = 0
	)

	// Update Alice's channel policy to set the new max_htlc value.
	req := &lnrpc.PolicyUpdateRequest{
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
		MaxHtlcMsat:   maxHtlcMsat,
		MinHtlcMsat:   minHtlcMsat,
		BaseFeeMsat:   baseFeeMsat,
		FeeRate:       feeRate,
		TimeLockDelta: timeLockDelta,
	}
	alice.RPC.UpdateChannelPolicy(req)

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      baseFeeMsat,
		FeeRateMilliMsat: feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          minHtlcMsat,
		MaxHtlcMsat:      maxHtlcMsat,
	}

	// Wait for the policy update to propagate to Bob.
	ht.AssertChannelPolicyUpdate(
		bob, alice, expectedPolicy, chanPoint, false,
	)

	// Create an invoice for an amount greater than the max htlc value.
	invoiceAmt := int64(maxHtlcMsat + 10_000_000)
	invoice := &lnrpc.Invoice{ValueMsat: invoiceAmt}
	resp := bob.RPC.AddInvoice(invoice)

	// Attempt to pay the invoice from Alice. The payment should be
	// splitted into two parts, one for the max_htlc value and one for the
	// remaining amount and succeed.
	payReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAssertSettled(alice, payReq)
}
