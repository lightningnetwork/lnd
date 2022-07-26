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
