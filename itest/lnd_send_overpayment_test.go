package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testSendPaymentOverpay verifies that a payer can intentionally overpay a
// fixed-amount BOLT11 invoice by specifying a larger amt_msat in the send
// request.
func testSendPaymentOverpay(ht *lntest.HarnessTest) {
	// Open a channel with 100k satoshis between Alice and Bob.
	chanAmt := btcutil.Amount(100_000)
	_, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil},
		lntest.OpenChannelParams{Amt: chanAmt},
	)
	alice, bob := nodes[0], nodes[1]

	const invoiceAmtSat = 1_000
	const overpayAmtSat = 1_100
	const overpayAmtMsat = overpayAmtSat * 1000

	// Create a fixed-amount invoice on Bob's side.
	invoice := bob.RPC.AddInvoice(&lnrpc.Invoice{
		Memo:  "overpay-test",
		Value: invoiceAmtSat,
	})

	// Alice sends a payment with amt_msat larger than the invoice
	// amount. This should succeed and deliver the overpaid amount.
	payment := ht.SendPaymentAssertSettled(
		alice, &routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			AmtMsat:        overpayAmtMsat,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// The payment value should reflect the overpaid amount.
	require.Equal(
		ht, int64(overpayAmtMsat), payment.ValueMsat,
		"value_msat should equal the overpaid amount",
	)

	// The invoice on Bob's side should show the overpaid amount.
	dbInvoice := bob.RPC.LookupInvoice(invoice.RHash)
	require.Equal(
		ht, lnrpc.Invoice_SETTLED, dbInvoice.State,
	)
	require.Equal(
		ht, int64(overpayAmtMsat), dbInvoice.AmtPaidMsat,
		"Bob should receive the overpaid amount",
	)
}
