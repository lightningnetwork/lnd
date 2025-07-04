package itest

import (
	"bytes"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func testDeleteCanceledInvoices(ht *lntest.HarnessTest) {
	// Open a channel with 100k satoshis between Alice and Bob.
	chanAmt := btcutil.Amount(100000)
	_, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil}, lntest.OpenChannelParams{Amt: chanAmt},
	)

	const paymentAmt = 1000
	bob := nodes[1]

	// Now that the channel is open, create 6 invoices for Bob.
	const numInvoices = 6
	invoiceResp := make([]*lnrpc.AddInvoiceResponse, numInvoices)

	for i := range numInvoices {
		// Create a unique 32-byte preimage for each invoice.
		preimage := bytes.Repeat([]byte{byte(i + 1)}, 32)

		invoice := &lnrpc.Invoice{
			RPreimage: preimage,
			Value:     paymentAmt,
		}

		invoiceResp[i] = bob.RPC.AddInvoice(invoice)
	}

	// Cancel all invoices except one.
	for i := range numInvoices - 1 {
		bob.RPC.CancelInvoice(invoiceResp[i].RHash)
	}

	// Let's assert an error when setting AllInvoices to true and providing
	// a list of invoice hashes.
	hashStrs := []string{lntypes.Hash(invoiceResp[0].RHash).String()}

	bob.RPC.DeleteCanceledInvoicesAssertErr(&lnrpc.DeleteInvoicesRequest{
		AllInvoices:   true,
		InvoiceHashes: hashStrs},
		"cannot use --all and --invoice_hashes at the same time",
	)

	// Let's assert an error while deleting the not canceled invoice.
	hashStrs = []string{
		lntypes.Hash(invoiceResp[numInvoices-1].RHash).String(),
	}

	bob.RPC.DeleteCanceledInvoicesAssertErr(
		&lnrpc.DeleteInvoicesRequest{InvoiceHashes: hashStrs},
		"no canceled invoices to delete",
	)

	// Let's delete one canceled invoice.
	hashStrs = []string{
		lntypes.Hash(invoiceResp[numInvoices-2].RHash).String(),
	}

	resp := bob.RPC.DeleteCanceledInvoices(
		&lnrpc.DeleteInvoicesRequest{InvoiceHashes: hashStrs},
	)

	require.Contains(ht, resp.String(), "1 canceled invoice(s) deleted")
	respList := bob.RPC.ListInvoices(&lnrpc.ListInvoiceRequest{})

	// Assert that the 1 invoice was deleted.
	require.Len(ht, respList.Invoices, numInvoices-1)

	// Let's try to delete the following list of invoices:
	// - an invoice with an invalid hash.
	// - a non existing invoice (the invoice that was already deleted).
	// - two canceled invoices.
	hashStrs = []string{
		"invalidhash",
		lntypes.Hash(invoiceResp[numInvoices-2].RHash).String(),
		lntypes.Hash(invoiceResp[numInvoices-6].RHash).String(),
		lntypes.Hash(invoiceResp[numInvoices-5].RHash).String(),
	}

	resp = bob.RPC.DeleteCanceledInvoices(
		&lnrpc.DeleteInvoicesRequest{InvoiceHashes: hashStrs},
	)

	require.Contains(ht, resp.String(), "2 canceled invoice(s) deleted")
	respList = bob.RPC.ListInvoices(&lnrpc.ListInvoiceRequest{})

	// Assert that the 2 other invoices were deleted (total of 3 deleted).
	require.Len(ht, respList.Invoices, numInvoices-3)

	// Finally let's delete all canceled invoices.
	bob.RPC.DeleteCanceledInvoices(
		&lnrpc.DeleteInvoicesRequest{AllInvoices: true},
	)

	respList = bob.RPC.ListInvoices(&lnrpc.ListInvoiceRequest{})

	// Assert that just one invoice exist (total of 5 deleted).
	require.Len(ht, respList.Invoices, numInvoices-5)

	// Assert that the invoiceResp with index 5 is still present and OPEN.
	require.Equal(ht, invoiceResp[numInvoices-1].RHash,
		respList.Invoices[0].RHash,
	)

	require.Equal(ht, lnrpc.Invoice_OPEN, respList.Invoices[0].State)
}
