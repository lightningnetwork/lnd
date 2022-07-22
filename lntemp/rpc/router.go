package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
)

// =====================
// RouterClient related RPCs.
// =====================

// UpdateChanStatus makes a UpdateChanStatus RPC call to node's RouterClient
// and asserts.
func (h *HarnessRPC) UpdateChanStatus(
	req *routerrpc.UpdateChanStatusRequest) *routerrpc.UpdateChanStatusResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Router.UpdateChanStatus(ctxt, req)
	h.NoError(err, "UpdateChanStatus")

	return resp
}
