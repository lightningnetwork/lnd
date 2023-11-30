package itest

import (
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testTrackPayments tests whether a client that calls the TrackPayments api
// receives payment updates.
func testTrackPayments(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob

	// Restart Alice with the new flag so she understands the new payment
	// status.
	ht.RestartNodeWithExtraArgs(alice, []string{
		"--routerrpc.usestatusinitiated",
	})

	// Open a channel between alice and bob.
	ht.EnsureConnected(alice, bob)
	channel := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: btcutil.Amount(300000),
		},
	)

	// Call the TrackPayments api to listen for payment updates.
	req := &routerrpc.TrackPaymentsRequest{
		NoInflightUpdates: false,
	}
	tracker := alice.RPC.TrackPayments(req)

	// Create an invoice from bob.
	var amountMsat int64 = 1000
	invoiceResp := bob.RPC.AddInvoice(
		&lnrpc.Invoice{
			ValueMsat: amountMsat,
		},
	)
	invoice := bob.RPC.LookupInvoice(invoiceResp.RHash)

	// Send payment from alice to bob.
	paymentClient := alice.RPC.SendPayment(
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
		},
	)

	// Make sure the payment doesn't error due to invalid parameters or so.
	_, err := paymentClient.Recv()
	require.NoError(ht, err, "unable to get payment update")

	// Assert the first payment update is an initiated update.
	update1, err := tracker.Recv()
	require.NoError(ht, err, "unable to receive payment update 1")

	require.Equal(ht, lnrpc.Payment_INITIATED, update1.Status)
	require.Equal(ht, lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
		update1.FailureReason)
	require.Equal(ht, invoice.PaymentRequest, update1.PaymentRequest)
	require.Equal(ht, amountMsat, update1.ValueMsat)

	// Assert the first payment update is an inflight update.
	update2, err := tracker.Recv()
	require.NoError(ht, err, "unable to receive payment update 2")

	require.Equal(ht, lnrpc.Payment_IN_FLIGHT, update2.Status)
	require.Equal(ht, lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
		update2.FailureReason)
	require.Equal(ht, invoice.PaymentRequest, update2.PaymentRequest)
	require.Equal(ht, amountMsat, update2.ValueMsat)

	// Assert the third payment update is a payment success update.
	update3, err := tracker.Recv()
	require.NoError(ht, err, "unable to receive payment update 3")

	require.Equal(ht, lnrpc.Payment_SUCCEEDED, update3.Status)
	require.Equal(ht, lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
		update3.FailureReason)
	require.Equal(ht, invoice.PaymentRequest, update3.PaymentRequest)
	require.Equal(ht, amountMsat, update3.ValueMsat)
	require.Equal(ht, hex.EncodeToString(invoice.RPreimage),
		update3.PaymentPreimage)

	// TODO(yy): remove the sleep once the following bug is fixed.
	// When the invoice is reported settled, the commitment dance is not
	// yet finished, which can cause an error when closing the channel,
	// saying there's active HTLCs. We need to investigate this issue and
	// reverse the order to, first finish the commitment dance, then report
	// the invoice as settled.
	time.Sleep(2 * time.Second)

	ht.CloseChannel(alice, channel)
}

// testTrackPaymentsCompatible checks that when `routerrpc.usestatusinitiated`
// is not set, the new Payment_INITIATED is replaced with Payment_IN_FLIGHT.
func testTrackPaymentsCompatible(ht *lntest.HarnessTest) {
	// Open a channel between alice and bob.
	alice, bob := ht.Alice, ht.Bob
	channel := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: btcutil.Amount(300000),
		},
	)

	// Call the TrackPayments api to listen for payment updates.
	req := &routerrpc.TrackPaymentsRequest{
		NoInflightUpdates: false,
	}
	tracker := alice.RPC.TrackPayments(req)

	// Create an invoice from bob.
	var amountMsat int64 = 1000
	invoiceResp := bob.RPC.AddInvoice(
		&lnrpc.Invoice{
			ValueMsat: amountMsat,
		},
	)
	invoice := bob.RPC.LookupInvoice(invoiceResp.RHash)

	// Send payment from alice to bob.
	paymentClient := alice.RPC.SendPayment(
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
		},
	)

	// Assert the first track payment update is an inflight update.
	update1, err := tracker.Recv()
	require.NoError(ht, err, "unable to receive payment update 1")
	require.Equal(ht, lnrpc.Payment_IN_FLIGHT, update1.Status)

	// Assert the first track payment update is an inflight update.
	update2, err := tracker.Recv()
	require.NoError(ht, err, "unable to receive payment update 2")
	require.Equal(ht, lnrpc.Payment_IN_FLIGHT, update2.Status)

	// Assert the third track payment update is a payment success update.
	update3, err := tracker.Recv()
	require.NoError(ht, err, "unable to receive payment update 3")
	require.Equal(ht, lnrpc.Payment_SUCCEEDED, update3.Status)

	// Assert the first payment client update is an inflight update.
	payment1, err := paymentClient.Recv()
	require.NoError(ht, err, "unable to get payment update")
	require.Equal(ht, lnrpc.Payment_IN_FLIGHT, payment1.Status)

	// Assert the second payment client update is an inflight update.
	payment2, err := paymentClient.Recv()
	require.NoError(ht, err, "unable to get payment update")
	require.Equal(ht, lnrpc.Payment_IN_FLIGHT, payment2.Status)

	// Assert the third payment client update is a success update.
	payment3, err := paymentClient.Recv()
	require.NoError(ht, err, "unable to get payment update")
	require.Equal(ht, lnrpc.Payment_SUCCEEDED, payment3.Status)

	// TODO(yy): remove the sleep once the following bug is fixed.
	// When the invoice is reported settled, the commitment dance is not
	// yet finished, which can cause an error when closing the channel,
	// saying there's active HTLCs. We need to investigate this issue and
	// reverse the order to, first finish the commitment dance, then report
	// the invoice as settled.
	time.Sleep(2 * time.Second)

	ht.CloseChannel(alice, channel)
}
