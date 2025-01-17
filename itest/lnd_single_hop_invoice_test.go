package itest

import (
	"bytes"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

func testSingleHopInvoice(ht *lntest.HarnessTest) {
	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil}, lntest.OpenChannelParams{Amt: chanAmt},
	)
	cp := chanPoints[0]
	alice, bob := nodes[0], nodes[1]

	// assertAmountPaid is a helper closure that asserts the amount paid by
	// Alice and received by Bob are expected.
	assertAmountPaid := func(expected int64) {
		ht.AssertAmountPaid("alice -> bob", alice, cp, expected, 0)
		ht.AssertAmountPaid("bob <- alice", bob, cp, 0, expected)
	}

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("A"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp := bob.RPC.AddInvoice(invoice)

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	ht.CompletePaymentRequests(alice, []string{invoiceResp.PaymentRequest})

	// Bob's invoice should now be found and marked as settled.
	dbInvoice := bob.RPC.LookupInvoice(invoiceResp.RHash)
	require.Equal(ht, lnrpc.Invoice_SETTLED, dbInvoice.State,
		"bob's invoice should be marked as settled")

	// With the payment completed all balance related stats should be
	// properly updated.
	assertAmountPaid(paymentAmt)

	// Create another invoice for Bob, this time leaving off the preimage
	// to one will be randomly generated. We'll test the proper
	// encoding/decoding of the zpay32 payment requests.
	invoice = &lnrpc.Invoice{
		Memo:  "test3",
		Value: paymentAmt,
	}
	invoiceResp = bob.RPC.AddInvoice(invoice)

	// Next send another payment, but this time using a zpay32 encoded
	// invoice rather than manually specifying the payment details.
	ht.CompletePaymentRequests(alice, []string{invoiceResp.PaymentRequest})

	// The second payment should also have succeeded, with the balances
	// being update accordingly.
	assertAmountPaid(paymentAmt * 2)

	// Next send a keysend payment.
	keySendPreimage := lntypes.Preimage{3, 4, 5, 11}
	keySendHash := keySendPreimage.Hash()

	req := &routerrpc.SendPaymentRequest{
		Dest:           bob.PubKey[:],
		Amt:            paymentAmt,
		FinalCltvDelta: 40,
		PaymentHash:    keySendHash[:],
		DestCustomRecords: map[uint64][]byte{
			record.KeySendType: keySendPreimage[:],
		},
		FeeLimitMsat: noFeeLimitMsat,
	}
	ht.SendPaymentAssertSettled(alice, req)

	// The keysend payment should also have succeeded, with the balances
	// being update accordingly.
	assertAmountPaid(paymentAmt * 3)

	// Assert that the invoice has the proper AMP fields set, since the
	// legacy keysend payment should have been promoted into an AMP payment
	// internally.
	keysendInvoice := bob.RPC.LookupInvoice(keySendHash[:])
	require.Len(ht, keysendInvoice.Htlcs, 1)
	htlc := keysendInvoice.Htlcs[0]
	require.Zero(ht, htlc.MppTotalAmtMsat)
	require.Nil(ht, htlc.Amp)

	// Now create an invoice and specify routing hints.
	// We will test that the routing hints are encoded properly.
	hintChannel := lnwire.ShortChannelID{BlockHeight: 10}
	bobPubKey := hex.EncodeToString(bob.PubKey[:])
	hint := &lnrpc.HopHint{
		NodeId:                    bobPubKey,
		ChanId:                    hintChannel.ToUint64(),
		FeeBaseMsat:               1,
		FeeProportionalMillionths: 1000000,
		CltvExpiryDelta:           20,
	}
	hints := []*lnrpc.RouteHint{{HopHints: []*lnrpc.HopHint{hint}}}

	invoice = &lnrpc.Invoice{
		Memo:       "hints",
		Value:      paymentAmt,
		RouteHints: hints,
	}

	invoiceResp = bob.RPC.AddInvoice(invoice)
	payreq := bob.RPC.DecodePayReq(invoiceResp.PaymentRequest)
	require.Len(ht, payreq.RouteHints, 1, "expected one routing hint")
	routingHint := payreq.RouteHints[0]
	require.Len(ht, routingHint.HopHints, 1, "expected one hop hint")

	hopHint := routingHint.HopHints[0]
	require.EqualValues(ht, 1000000, hopHint.FeeProportionalMillionths,
		"wrong FeeProportionalMillionths")
	require.Equal(ht, bobPubKey, hopHint.NodeId, "wrong NodeId")
	require.Equal(ht, hintChannel.ToUint64(), hopHint.ChanId,
		"wrong ChanId")
	require.EqualValues(ht, 1, hopHint.FeeBaseMsat, "wrong FeeBaseMsat")
	require.EqualValues(ht, 20, hopHint.CltvExpiryDelta,
		"wrong CltvExpiryDelta")
}
