package itest

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testHoldInvoicePersistence tests that a sender to a hold-invoice, can be
// restarted before the payment gets settled, and still be able to receive the
// preimage.
func testHoldInvoicePersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt     = btcutil.Amount(1000000)
		numPayments = 10
	)

	// Create carol, and clean up when the test finishes.
	carol := net.NewNode(t.t, "Carol", nil)
	defer shutdownAndAssert(net, t, carol)

	// Connect Alice to Carol.
	net.ConnectNodes(t.t, net.Alice, carol)

	// Open a channel between Alice and Carol which is private so that we
	// cover the addition of hop hints for hold invoices.
	chanPointAlice := openChannelAndAssert(
		t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	// Wait for Alice and Carol to receive the channel edge from the
	// funding manager.
	err := net.Alice.WaitForNetworkChannelOpen(chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	err = carol.WaitForNetworkChannelOpen(chanPointAlice)
	if err != nil {
		t.Fatalf("carol didn't see the carol->alice channel before "+
			"timeout: %v", err)
	}

	// Create preimages for all payments we are going to initiate.
	var preimages []lntypes.Preimage
	for i := 0; i < numPayments; i++ {
		var preimage lntypes.Preimage
		_, err = rand.Read(preimage[:])
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		preimages = append(preimages, preimage)
	}

	// Let Carol create hold-invoices for all the payments.
	var (
		payAmt         = btcutil.Amount(4)
		payReqs        []string
		invoiceStreams []invoicesrpc.Invoices_SubscribeSingleInvoiceClient
	)

	for _, preimage := range preimages {
		payHash := preimage.Hash()

		// Make our invoices private so that we get coverage for adding
		// hop hints.
		invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
			Memo:    "testing",
			Value:   int64(payAmt),
			Hash:    payHash[:],
			Private: true,
		}
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		resp, err := carol.AddHoldInvoice(ctxt, invoiceReq)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		ctx, cancel := context.WithCancel(ctxb)
		defer cancel()

		stream, err := carol.SubscribeSingleInvoice(
			ctx,
			&invoicesrpc.SubscribeSingleInvoiceRequest{
				RHash: payHash[:],
			},
		)
		if err != nil {
			t.Fatalf("unable to subscribe to invoice: %v", err)
		}

		invoiceStreams = append(invoiceStreams, stream)
		payReqs = append(payReqs, resp.PaymentRequest)
	}

	// Wait for all the invoices to reach the OPEN state.
	for _, stream := range invoiceStreams {
		invoice, err := stream.Recv()
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		if invoice.State != lnrpc.Invoice_OPEN {
			t.Fatalf("expected OPEN, got state: %v", invoice.State)
		}
	}

	// Let Alice initiate payments for all the created invoices.
	var paymentStreams []routerrpc.Router_SendPaymentV2Client
	for _, payReq := range payReqs {
		ctx, cancel := context.WithCancel(ctxb)
		defer cancel()

		payStream, err := net.Alice.RouterClient.SendPaymentV2(
			ctx, &routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitSat:    1000000,
			},
		)
		if err != nil {
			t.Fatalf("unable to send alice htlc: %v", err)
		}

		paymentStreams = append(paymentStreams, payStream)
	}

	// Wait for inlight status update.
	for _, payStream := range paymentStreams {
		payment, err := payStream.Recv()
		if err != nil {
			t.Fatalf("Failed receiving status update: %v", err)
		}

		if payment.Status != lnrpc.Payment_IN_FLIGHT {
			t.Fatalf("state not in flight: %v", payment.Status)
		}
	}

	// The payments should now show up in Alice's ListInvoices, with a zero
	// preimage, indicating they are not yet settled.
	err = wait.NoError(func() error {
		req := &lnrpc.ListPaymentsRequest{
			IncludeIncomplete: true,
		}
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		paymentsResp, err := net.Alice.ListPayments(ctxt, req)
		if err != nil {
			return fmt.Errorf("error when obtaining payments: %v",
				err)
		}

		// Gather the payment hashes we are looking for in the
		// response.
		payHashes := make(map[string]struct{})
		for _, preimg := range preimages {
			payHashes[preimg.Hash().String()] = struct{}{}
		}

		var zeroPreimg lntypes.Preimage
		for _, payment := range paymentsResp.Payments {
			_, ok := payHashes[payment.PaymentHash]
			if !ok {
				continue
			}

			// The preimage should NEVER be non-zero at this point.
			if payment.PaymentPreimage != zeroPreimg.String() {
				t.Fatalf("expected zero preimage, got %v",
					payment.PaymentPreimage)
			}

			// We wait for the payment attempt to have been
			// properly recorded in the DB.
			if len(payment.Htlcs) == 0 {
				return fmt.Errorf("no attempt recorded")
			}

			delete(payHashes, payment.PaymentHash)
		}

		if len(payHashes) != 0 {
			return fmt.Errorf("payhash not found in response")
		}

		return nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("predicate not satisfied: %v", err)
	}

	// Wait for all invoices to be accepted.
	for _, stream := range invoiceStreams {
		invoice, err := stream.Recv()
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		if invoice.State != lnrpc.Invoice_ACCEPTED {
			t.Fatalf("expected ACCEPTED, got state: %v",
				invoice.State)
		}
	}

	// Restart alice. This to ensure she will still be able to handle
	// settling the invoices after a restart.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Now after a restart, we must re-track the payments. We set up a
	// goroutine for each to track thir status updates.
	var (
		statusUpdates []chan *lnrpc.Payment
		wg            sync.WaitGroup
		quit          = make(chan struct{})
	)

	defer close(quit)
	for _, preimg := range preimages {
		hash := preimg.Hash()

		ctx, cancel := context.WithCancel(ctxb)
		defer cancel()

		payStream, err := net.Alice.RouterClient.TrackPaymentV2(
			ctx, &routerrpc.TrackPaymentRequest{
				PaymentHash: hash[:],
			},
		)
		if err != nil {
			t.Fatalf("unable to send track payment: %v", err)
		}

		// We set up a channel where we'll forward any status update.
		upd := make(chan *lnrpc.Payment)
		wg.Add(1)
		go func() {
			defer wg.Done()

			for {
				payment, err := payStream.Recv()
				if err != nil {
					close(upd)
					return
				}

				select {
				case upd <- payment:
				case <-quit:
					return
				}
			}
		}()

		statusUpdates = append(statusUpdates, upd)
	}

	// Wait for the in-flight status update.
	for _, upd := range statusUpdates {
		select {
		case payment, ok := <-upd:
			if !ok {
				t.Fatalf("failed getting payment update")
			}

			if payment.Status != lnrpc.Payment_IN_FLIGHT {
				t.Fatalf("state not in in flight: %v",
					payment.Status)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("in flight status not recevied")
		}
	}

	// Settle invoices half the invoices, cancel the rest.
	for i, preimage := range preimages {
		var expectedState lnrpc.Invoice_InvoiceState

		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		if i%2 == 0 {
			settle := &invoicesrpc.SettleInvoiceMsg{
				Preimage: preimage[:],
			}
			_, err = carol.SettleInvoice(ctxt, settle)

			expectedState = lnrpc.Invoice_SETTLED
		} else {
			hash := preimage.Hash()
			settle := &invoicesrpc.CancelInvoiceMsg{
				PaymentHash: hash[:],
			}
			_, err = carol.CancelInvoice(ctxt, settle)

			expectedState = lnrpc.Invoice_CANCELED
		}
		if err != nil {
			t.Fatalf("unable to cancel/settle invoice: %v", err)
		}

		stream := invoiceStreams[i]
		invoice, err := stream.Recv()
		require.NoError(t.t, err)
		require.Equal(t.t, expectedState, invoice.State)
	}

	// Make sure we get the expected status update.
	for i, upd := range statusUpdates {
		// Read until the payment is in a terminal state.
		var payment *lnrpc.Payment
		for payment == nil {
			select {
			case p, ok := <-upd:
				if !ok {
					t.Fatalf("failed getting payment update")
				}

				if p.Status == lnrpc.Payment_IN_FLIGHT {
					continue
				}

				payment = p
			case <-time.After(5 * time.Second):
				t.Fatalf("in flight status not recevied")
			}
		}

		// Assert terminal payment state.
		if i%2 == 0 {
			if payment.Status != lnrpc.Payment_SUCCEEDED {
				t.Fatalf("state not succeeded : %v",
					payment.Status)
			}
		} else {
			if payment.FailureReason !=
				lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS {

				t.Fatalf("state not failed: %v",
					payment.FailureReason)
			}
		}
	}

	// Check that Alice's invoices to be shown as settled and failed
	// accordingly, and preimages matching up.
	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	paymentsResp, err := net.Alice.ListPayments(ctxt, req)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	for i, preimage := range preimages {
		paymentHash := preimage.Hash()
		var p string
		for _, resp := range paymentsResp.Payments {
			if resp.PaymentHash == paymentHash.String() {
				p = resp.PaymentPreimage
				break
			}
		}
		if p == "" {
			t.Fatalf("payment not found")
		}

		if i%2 == 0 {
			if p != preimage.String() {
				t.Fatalf("preimage doesn't match: %v vs %v",
					p, preimage.String())
			}
		} else {
			if p != lntypes.ZeroHash.String() {
				t.Fatalf("preimage not zero: %v", p)
			}
		}
	}

	// Check that all of our invoice streams are terminated by the server
	// since the invoices have completed.
	for _, stream := range invoiceStreams {
		_, err = stream.Recv()
		require.Equal(t.t, io.EOF, err)
	}
}
