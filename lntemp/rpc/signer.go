package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
)

// =====================
// Signer related RPCs.
// =====================

// SignOutputRaw makes a RPC call to node's SignOutputRaw and asserts.
func (h *HarnessRPC) SignOutputRaw(req *signrpc.SignReq) *signrpc.SignResp {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.SignOutputRaw(ctxt, req)
	h.NoError(err, "SignOutputRaw")

	return resp
}
