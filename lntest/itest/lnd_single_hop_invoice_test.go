// +build rpctest

package itest

import (
	"bytes"
	"context"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/record"
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
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
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
	sendReq := &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Ensure we obtain the proper preimage in the response.
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	} else if !bytes.Equal(preimage, resp.PaymentPreimage) {
		t.Fatalf("preimage mismatch: expected %v, got %v", preimage,
			resp.GetPaymentPreimage())
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
	sendReq = &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	}

	// The second payment should also have succeeded, with the balances
	// being update accordingly.
	err = wait.NoError(
		assertAmountSent(2*paymentAmt, net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Next send a key send payment.
	keySendPreimage := lntypes.Preimage{3, 4, 5, 11}
	keySendHash := keySendPreimage.Hash()

	sendReq = &lnrpc.SendRequest{
		Dest:           net.Bob.PubKey[:],
		Amt:            paymentAmt,
		FinalCltvDelta: 40,
		PaymentHash:    keySendHash[:],
		DestCustomRecords: map[uint64][]byte{
			record.KeySendType: keySendPreimage[:],
		},
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	}

	// The key send payment should also have succeeded, with the balances
	// being update accordingly.
	err = wait.NoError(
		assertAmountSent(3*paymentAmt, net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}
