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

type PaymentClient routerrpc.Router_SendPaymentV2Client

// SendPayment sends a payment using the given node and payment request. It
// also asserts the payment being sent successfully.
func (h *HarnessRPC) SendPayment(
	req *routerrpc.SendPaymentRequest) PaymentClient {

	// SendPayment needs to have the context alive for the entire test case
	// as the router relies on the context to propagate HTLCs. Thus we use
	// runCtx here instead of a timeout context.
	stream, err := h.Router.SendPaymentV2(h.runCtx, req)
	h.NoError(err, "SendPaymentV2")

	return stream
}
