package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testHoldInvoicePersistence tests that a sender to a hold-invoice, can be
// restarted before the payment gets settled, and still be able to receive the
// preimage.
func testHoldInvoicePersistence(ht *lntest.HarnessTest) {
	const (
		chanAmt     = btcutil.Amount(1000000)
		numPayments = 10
		reason      = lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS //nolint:ll
	)

	// Create carol, and clean up when the test finishes.
	carol := ht.NewNode("Carol", nil)

	// Connect Alice to Carol.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)
	ht.EnsureConnected(alice, bob)
	ht.ConnectNodes(alice, carol)

	// Open a channel between Alice and Carol which is private so that we
	// cover the addition of hop hints for hold invoices.
	ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	// For Carol to include her private channel with Alice as a hop hint,
	// we need Alice to be perceived as a "public" node, meaning that she
	// has at least one public channel in the graph. We open a public
	// channel from Alice -> Bob and wait for Carol to see it.
	chanPointBob := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Wait for Carol to see the open channel Alice-Bob.
	ht.AssertChannelInGraph(carol, chanPointBob)

	// Create preimages for all payments we are going to initiate.
	var preimages []lntypes.Preimage
	for i := 0; i < numPayments; i++ {
		var preimage lntypes.Preimage
		copy(preimage[:], ht.Random32Bytes())
		preimages = append(preimages, preimage)
	}

	// Let Carol create hold-invoices for all the payments.
	var (
		payAmt         = btcutil.Amount(4)
		payReqs        []string
		invoiceStreams []rpc.SingleInvoiceClient
	)

	assertInvoiceState := func(state lnrpc.Invoice_InvoiceState) {
		for _, client := range invoiceStreams {
			ht.AssertInvoiceState(client, state)
		}
	}

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
		resp := carol.RPC.AddHoldInvoice(invoiceReq)
		payReqs = append(payReqs, resp.PaymentRequest)

		// We expect all of our invoices to have hop hints attached,
		// since Carol and Alice are connected with a private channel.
		// We assert that we have one hop hint present to ensure that
		// we've got coverage for hop hints.
		invoice := alice.RPC.DecodePayReq(resp.PaymentRequest)
		require.Len(ht, invoice.RouteHints, 1)

		stream := carol.RPC.SubscribeSingleInvoice(payHash[:])
		invoiceStreams = append(invoiceStreams, stream)
	}

	// Wait for all the invoices to reach the OPEN state.
	assertInvoiceState(lnrpc.Invoice_OPEN)

	// Let Alice initiate payments for all the created invoices.
	for _, payReq := range payReqs {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			FeeLimitSat:    1000000,
		}

		// Wait for inflight status update.
		ht.SendPaymentAndAssertStatus(
			alice, req, lnrpc.Payment_IN_FLIGHT,
		)
	}

	// The payments should now show up in Alice's ListPayments, with a zero
	// preimage, indicating they are not yet settled.
	var zeroPreimg lntypes.Preimage
	err := wait.NoError(func() error {
		payments := ht.AssertNumPayments(alice, numPayments)

		// Gather the payment hashes we are looking for in the
		// response.
		payHashes := make(map[string]struct{})
		for _, preimg := range preimages {
			payHashes[preimg.Hash().String()] = struct{}{}
		}

		for _, payment := range payments {
			_, ok := payHashes[payment.PaymentHash]
			if !ok {
				continue
			}

			// The preimage should NEVER be non-zero at this point.
			require.Equal(ht, zeroPreimg.String(),
				payment.PaymentPreimage,
				"expected zero preimage")

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
	require.NoError(ht, err, "timeout checking alice's payments")

	// Wait for all invoices to be accepted.
	assertInvoiceState(lnrpc.Invoice_ACCEPTED)

	// Restart alice. This to ensure she will still be able to handle
	// settling the invoices after a restart.
	ht.RestartNode(alice)

	// Ensure the connections are made.
	//
	// TODO(yy): we shouldn't need these two lines since the connections
	// are permanent, they'd reconnect automatically upon Alice's restart.
	// However, we'd sometimes see the error `unable to gracefully close
	// channel while peer is offline (try force closing it instead):
	// channel link not found` from closing the channels in the end,
	// indicating there's something wrong with the peer conn. We need to
	// investigate and fix it in peer conn management.
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(alice, carol)

	// Now after a restart, we must re-track the payments. We set up a
	// goroutine for each to track their status updates.
	for _, preimg := range preimages {
		hash := preimg.Hash()

		payStream := alice.RPC.TrackPaymentV2(hash[:])
		ht.ReceiveTrackPayment(payStream)

		ht.AssertPaymentStatus(alice, hash, lnrpc.Payment_IN_FLIGHT)
	}

	// Settle invoices half the invoices, cancel the rest.
	for i, preimage := range preimages {
		if i%2 == 0 {
			carol.RPC.SettleInvoice(preimage[:])
			ht.AssertInvoiceState(
				invoiceStreams[i], lnrpc.Invoice_SETTLED,
			)
		} else {
			hash := preimage.Hash()
			carol.RPC.CancelInvoice(hash[:])
			ht.AssertInvoiceState(
				invoiceStreams[i], lnrpc.Invoice_CANCELED,
			)
		}
	}

	// Check that Alice's invoices to be shown as settled and failed
	// accordingly, and preimages matching up.
	for i, preimg := range preimages {
		if i%2 == 0 {
			ht.AssertPaymentStatus(
				alice, preimg.Hash(), lnrpc.Payment_SUCCEEDED,
			)
		} else {
			payment := ht.AssertPaymentStatus(
				alice, preimg.Hash(), lnrpc.Payment_FAILED,
			)
			require.Equal(ht, reason, payment.FailureReason,
				"wrong failure reason")
		}
	}
}
