package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/neutrinorpc"
)

// =====================
// NeutrinoKitClient related RPCs.
// =====================

// Status makes an RPC call to neutrino kit client's Status and asserts.
func (h *HarnessRPC) Status(
	req *neutrinorpc.StatusRequest) *neutrinorpc.StatusResponse {

	if req == nil {
		req = &neutrinorpc.StatusRequest{}
	}

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.NeutrinoKit.Status(ctxt, req)
	h.NoError(err, "Status")

	return resp
}

// GetCFilter makes an RPC call to neutrino kit client's GetCFilter and asserts.
func (h *HarnessRPC) GetCFilter(
	req *neutrinorpc.GetCFilterRequest) *neutrinorpc.GetCFilterResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.NeutrinoKit.GetCFilter(ctxt, req)
	h.NoError(err, "GetCFilter")

	return resp
}

// AddPeer makes an RPC call to neutrino kit client's AddPeer and asserts.
func (h *HarnessRPC) AddPeer(
	req *neutrinorpc.AddPeerRequest) *neutrinorpc.AddPeerResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.NeutrinoKit.AddPeer(ctxt, req)
	h.NoError(err, "AddPeer")

	return resp
}
