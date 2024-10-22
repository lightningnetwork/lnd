package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
)

// =====================
// InvoiceClient related RPCs.
// =====================

// LookupInvoiceV2 queries the node's invoices using the invoice client's
// LookupInvoiceV2.
func (h *HarnessRPC) LookupInvoiceV2(
	req *invoicesrpc.LookupInvoiceMsg) *lnrpc.Invoice {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Invoice.LookupInvoiceV2(ctxt, req)
	h.NoError(err, "LookupInvoiceV2")

	return resp
}

// AddHoldInvoice adds a hold invoice for the given node and asserts.
func (h *HarnessRPC) AddHoldInvoice(
	r *invoicesrpc.AddHoldInvoiceRequest) *invoicesrpc.AddHoldInvoiceResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	invoice, err := h.Invoice.AddHoldInvoice(ctxt, r)
	h.NoError(err, "AddHoldInvoice")

	return invoice
}

// SettleInvoice settles a given invoice and asserts.
func (h *HarnessRPC) SettleInvoice(
	preimage []byte) *invoicesrpc.SettleInvoiceResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &invoicesrpc.SettleInvoiceMsg{Preimage: preimage}
	resp, err := h.Invoice.SettleInvoice(ctxt, req)
	h.NoError(err, "SettleInvoice")

	return resp
}

// CancelInvoice cancels a given invoice and asserts.
func (h *HarnessRPC) CancelInvoice(
	payHash []byte) *invoicesrpc.CancelInvoiceResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &invoicesrpc.CancelInvoiceMsg{PaymentHash: payHash}
	resp, err := h.Invoice.CancelInvoice(ctxt, req)
	h.NoError(err, "CancelInvoice")

	return resp
}

type SingleInvoiceClient invoicesrpc.Invoices_SubscribeSingleInvoiceClient

// SubscribeSingleInvoice creates a subscription client for given invoice and
// asserts its creation.
func (h *HarnessRPC) SubscribeSingleInvoice(rHash []byte) SingleInvoiceClient {
	req := &invoicesrpc.SubscribeSingleInvoiceRequest{RHash: rHash}

	// SubscribeSingleInvoice needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	client, err := h.Invoice.SubscribeSingleInvoice(h.runCtx, req)
	h.NoError(err, "SubscribeSingleInvoice")

	return client
}

type InvoiceHtlcModifierClient invoicesrpc.Invoices_HtlcModifierClient

// InvoiceHtlcModifier makes an RPC call to the node's RouterClient and asserts.
func (h *HarnessRPC) InvoiceHtlcModifier() (InvoiceHtlcModifierClient,
	context.CancelFunc) {

	// InvoiceHtlcModifier needs to have the context alive for the entire
	// test case as the returned client will be used for send and receive
	// events stream. Therefore, we use cancel context here instead of a
	// timeout context.
	ctxt, cancel := context.WithCancel(h.runCtx)
	resp, err := h.Invoice.HtlcModifier(ctxt)
	h.NoError(err, "InvoiceHtlcModifier")

	return resp, cancel
}
