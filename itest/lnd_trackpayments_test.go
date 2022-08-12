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

	// Make sure the payment doesn't error due to invalid parameters or so.
	_, err := paymentClient.Recv()
	require.NoError(ht, err, "unable to get payment update.")

	// Assert the first payment update is an inflight update.
	update1, err := tracker.Recv()
	require.NoError(ht, err, "unable to receive payment update 1.")

	require.Equal(
		ht, lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
		update1.FailureReason,
	)
	require.Equal(ht, lnrpc.Payment_IN_FLIGHT, update1.Status)
	require.Equal(ht, invoice.PaymentRequest, update1.PaymentRequest)
	require.Equal(ht, amountMsat, update1.ValueMsat)

	// Assert the second payment update is a payment success update.
	update2, err := tracker.Recv()
	require.NoError(ht, err, "unable to receive payment update 2.")

	require.Equal(
		ht, lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
		update2.FailureReason,
	)
	require.Equal(ht, lnrpc.Payment_SUCCEEDED, update2.Status)
	require.Equal(ht, invoice.PaymentRequest, update2.PaymentRequest)
	require.Equal(ht, amountMsat, update2.ValueMsat)
	require.Equal(
		ht, hex.EncodeToString(invoice.RPreimage),
		update2.PaymentPreimage,
	)

	// TODO(yy): remove the sleep once the following bug is fixed.
	// When the invoice is reported settled, the commitment dance is not
	// yet finished, which can cause an error when closing the channel,
	// saying there's active HTLCs. We need to investigate this issue and
	// reverse the order to, first finish the commitment dance, then report
	// the invoice as settled.
	time.Sleep(2 * time.Second)

	ht.CloseChannel(alice, channel)
}
