package rpc

import (
	"github.com/lightningnetwork/lnd/lnrpc"
)

// =====================
// StateClient related RPCs.
// =====================

// SubscribeState makes a rpc call to StateClient and asserts there's no error.
func (h *HarnessRPC) SubscribeState() lnrpc.State_SubscribeStateClient {
	client, err := h.State.SubscribeState(
		h.runCtx, &lnrpc.SubscribeStateRequest{},
	)
	h.NoError(err, "SubscribeState")

	return client
}
