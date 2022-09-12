package itest

import (
	"context"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testTrackPayments tests whether a client that calls the TrackPayments api
// receives payment updates.
func testTrackPayments(net *lntest.NetworkHarness, t *harnessTest) {
	// Open a channel between alice and bob.
	net.EnsureConnected(t.t, net.Alice, net.Bob)
	channel := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: btcutil.Amount(300000),
		},
	)
	defer closeChannelAndAssert(t, net, net.Alice, channel, true)

	err := net.Alice.WaitForNetworkChannelOpen(channel)
	require.NoError(t.t, err, "unable to wait for channel to open")

	ctxb := context.Background()
	ctxt, cancelTracker := context.WithCancel(ctxb)
	defer cancelTracker()

	// Call the TrackPayments api to listen for payment updates.
	tracker, err := net.Alice.RouterClient.TrackPayments(
		ctxt,
		&routerrpc.TrackPaymentsRequest{
			NoInflightUpdates: false,
		},
	)
	require.NoError(t.t, err, "failed to call TrackPayments successfully.")

	// Create an invoice from bob.
	var amountMsat int64 = 1000
	invoiceResp, err := net.Bob.AddInvoice(
		ctxb,
		&lnrpc.Invoice{
			ValueMsat: amountMsat,
		},
	)
	require.NoError(t.t, err, "unable to add invoice.")

	invoice, err := net.Bob.LookupInvoice(
		ctxb,
		&lnrpc.PaymentHash{
			RHashStr: hex.EncodeToString(invoiceResp.RHash),
		},
	)
	require.NoError(t.t, err, "unable to find invoice.")

	// Send payment from alice to bob.
	paymentClient, err := net.Alice.RouterClient.SendPaymentV2(
		ctxb,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
		},
	)
	require.NoError(t.t, err, "unable to send payment.")

	// Make sure the payment doesn't error due to invalid parameters or so.
	_, err = paymentClient.Recv()
	require.NoError(t.t, err, "unable to get payment update.")

	// Assert the first payment update is an inflight update.
	update1, err := tracker.Recv()
	require.NoError(t.t, err, "unable to receive payment update 1.")

	require.Equal(
		t.t, lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
		update1.FailureReason,
	)
	require.Equal(t.t, lnrpc.Payment_IN_FLIGHT, update1.Status)
	require.Equal(t.t, invoice.PaymentRequest, update1.PaymentRequest)
	require.Equal(t.t, amountMsat, update1.ValueMsat)

	// Assert the second payment update is a payment success update.
	update2, err := tracker.Recv()
	require.NoError(t.t, err, "unable to receive payment update 2.")

	require.Equal(
		t.t, lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
		update2.FailureReason,
	)
	require.Equal(t.t, lnrpc.Payment_SUCCEEDED, update2.Status)
	require.Equal(t.t, invoice.PaymentRequest, update2.PaymentRequest)
	require.Equal(t.t, amountMsat, update2.ValueMsat)
	require.Equal(
		t.t, hex.EncodeToString(invoice.RPreimage),
		update2.PaymentPreimage,
	)
}
