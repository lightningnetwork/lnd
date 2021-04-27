package itest

import (
	"bytes"
	"context"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

func testSingleHopInvoice(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// Open a channel with 100k satoshis between Alice and Bob with Alice being
	// the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanAmt := btcutil.Amount(100000)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

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
	invoiceResp, err := net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for Alice to recognize and advertise the new channel generated
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp := sendAndAssertSuccess(
		ctxt, t, net.Alice,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)
	if hex.EncodeToString(preimage) != resp.PaymentPreimage {
		t.Fatalf("preimage mismatch: expected %v, got %v", preimage,
			resp.PaymentPreimage)
	}

	// Bob's invoice should now be found and marked as settled.
	payHash := &lnrpc.PaymentHash{
		RHash: invoiceResp.RHash,
	}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	dbInvoice, err := net.Bob.LookupInvoice(ctxt, payHash)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}
	if !dbInvoice.Settled {
		t.Fatalf("bob's invoice should be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// With the payment completed all balance related stats should be
	// properly updated.
	err = wait.NoError(
		assertAmountSent(paymentAmt, net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Create another invoice for Bob, this time leaving off the preimage
	// to one will be randomly generated. We'll test the proper
	// encoding/decoding of the zpay32 payment requests.
	invoice = &lnrpc.Invoice{
		Memo:  "test3",
		Value: paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	invoiceResp, err = net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Next send another payment, but this time using a zpay32 encoded
	// invoice rather than manually specifying the payment details.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sendAndAssertSuccess(
		ctxt, t, net.Alice,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// The second payment should also have succeeded, with the balances
	// being update accordingly.
	err = wait.NoError(
		assertAmountSent(2*paymentAmt, net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Next send a keysend payment.
	keySendPreimage := lntypes.Preimage{3, 4, 5, 11}
	keySendHash := keySendPreimage.Hash()

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sendAndAssertSuccess(
		ctxt, t, net.Alice,
		&routerrpc.SendPaymentRequest{
			Dest:           net.Bob.PubKey[:],
			Amt:            paymentAmt,
			FinalCltvDelta: 40,
			PaymentHash:    keySendHash[:],
			DestCustomRecords: map[uint64][]byte{
				record.KeySendType: keySendPreimage[:],
			},
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// The keysend payment should also have succeeded, with the balances
	// being update accordingly.
	err = wait.NoError(
		assertAmountSent(3*paymentAmt, net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Assert that the invoice has the proper AMP fields set, since the
	// legacy keysend payment should have been promoted into an AMP payment
	// internally.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	keysendInvoice, err := net.Bob.LookupInvoice(
		ctxt, &lnrpc.PaymentHash{
			RHash: keySendHash[:],
		},
	)
	require.NoError(t.t, err)
	require.Equal(t.t, 1, len(keysendInvoice.Htlcs))
	htlc := keysendInvoice.Htlcs[0]
	require.Equal(t.t, uint64(0), htlc.MppTotalAmtMsat)
	require.Nil(t.t, htlc.Amp)

	// Now create an invoice and specify routing hints.
	// We will test that the routing hints are encoded properly.
	hintChannel := lnwire.ShortChannelID{BlockHeight: 10}
	bobPubKey := hex.EncodeToString(net.Bob.PubKey[:])
	hints := []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{
				{
					NodeId:                    bobPubKey,
					ChanId:                    hintChannel.ToUint64(),
					FeeBaseMsat:               1,
					FeeProportionalMillionths: 1000000,
					CltvExpiryDelta:           20,
				},
			},
		},
	}

	invoice = &lnrpc.Invoice{
		Memo:       "hints",
		Value:      paymentAmt,
		RouteHints: hints,
	}

	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	invoiceResp, err = net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}
	payreq, err := net.Bob.DecodePayReq(ctxt, &lnrpc.PayReqString{PayReq: invoiceResp.PaymentRequest})
	if err != nil {
		t.Fatalf("failed to decode payment request %v", err)
	}
	if len(payreq.RouteHints) != 1 {
		t.Fatalf("expected one routing hint")
	}
	routingHint := payreq.RouteHints[0]
	if len(routingHint.HopHints) != 1 {
		t.Fatalf("expected one hop hint")
	}
	hopHint := routingHint.HopHints[0]
	if hopHint.FeeProportionalMillionths != 1000000 {
		t.Fatalf("wrong FeeProportionalMillionths %v",
			hopHint.FeeProportionalMillionths)
	}
	if hopHint.NodeId != bobPubKey {
		t.Fatalf("wrong NodeId %v",
			hopHint.NodeId)
	}
	if hopHint.ChanId != hintChannel.ToUint64() {
		t.Fatalf("wrong ChanId %v",
			hopHint.ChanId)
	}
	if hopHint.FeeBaseMsat != 1 {
		t.Fatalf("wrong FeeBaseMsat %v",
			hopHint.FeeBaseMsat)
	}
	if hopHint.CltvExpiryDelta != 20 {
		t.Fatalf("wrong CltvExpiryDelta %v",
			hopHint.CltvExpiryDelta)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}
