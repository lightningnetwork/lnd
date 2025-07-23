package itest

import (
	"bytes"

	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func testDeleteCanceledInvoice(ht *lntest.HarnessTest) {
	bob := ht.NewNode("bob", nil)

	// Create 2 invoices for Bob.
	const numInvoices = 2
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

	// Cancel all invoices except the last one.
	for i := range numInvoices - 1 {
		bob.RPC.CancelInvoice(invoiceResp[i].RHash)
	}

	// Let's assert an error while providing no invoice hash.
	bob.RPC.DeleteCanceledInvoiceAssertErr(
		&lnrpc.DelCanceledInvoiceReq{},
		invoices.ErrNoInvoiceHash.Error(),
	)

	// Let's assert an error while providing a hash with an invalid length.
	bob.RPC.DeleteCanceledInvoiceAssertErr(
		&lnrpc.DelCanceledInvoiceReq{InvoiceHash: "bb02fbfa62983b6b62"},
		"invalid hash string length",
	)

	// Let's assert an error is returned for a hash with invalid encoding.
	bob.RPC.DeleteCanceledInvoiceAssertErr(
		&lnrpc.DelCanceledInvoiceReq{
			InvoiceHash: "bb02fbfa62983b6b621376bf8230732dd3a6dce" +
				"a9f5df803c0935ae6ce7440dg",
		}, "encoding/hex: invalid byte",
	)

	// Let's delete one canceled invoice.
	invoiceToDelete := lntypes.Hash(invoiceResp[0].RHash).String()
	resp := bob.RPC.DeleteCanceledInvoice(
		&lnrpc.DelCanceledInvoiceReq{InvoiceHash: invoiceToDelete},
	)

	require.Contains(
		ht, resp.String(), "canceled invoice deleted successfully",
	)

	respList := bob.RPC.ListInvoices(&lnrpc.ListInvoiceRequest{})

	// Assert that the invoice was deleted.
	require.Len(ht, respList.Invoices, numInvoices-1)

	// Let's assert an error while deleting a non-existent invoice.
	bob.RPC.DeleteCanceledInvoiceAssertErr(
		&lnrpc.DelCanceledInvoiceReq{InvoiceHash: invoiceToDelete},
		invoices.ErrInvoiceNotFound.Error(),
	)

	// Let's assert an error while deleting a non canceled invoice.
	notCanceledInvoice := lntypes.Hash(invoiceResp[numInvoices-1].RHash).
		String()

	bob.RPC.DeleteCanceledInvoiceAssertErr(
		&lnrpc.DelCanceledInvoiceReq{InvoiceHash: notCanceledInvoice},
		invoices.ErrInvoiceNotCanceled.Error(),
	)

	// Assert that the last invoice in the invoiceResp is still present and
	// OPEN.
	respList = bob.RPC.ListInvoices(&lnrpc.ListInvoiceRequest{})
	require.Equal(
		ht, invoiceResp[numInvoices-1].RHash,
		respList.Invoices[0].RHash,
	)

	require.Equal(ht, lnrpc.Invoice_OPEN, respList.Invoices[0].State)

	// Assert that just one invoice exists.
	require.Len(ht, respList.Invoices, 1)
}
