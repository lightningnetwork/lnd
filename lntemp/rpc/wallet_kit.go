package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// =====================
// WalletKitClient related RPCs.
// =====================

// FinalizePsbt makes a RPC call to node's ListUnspent and asserts.
func (h *HarnessRPC) ListUnspent(
	req *walletrpc.ListUnspentRequest) *walletrpc.ListUnspentResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.ListUnspent(ctxt, req)
	h.NoError(err, "ListUnspent")

	return resp
}
