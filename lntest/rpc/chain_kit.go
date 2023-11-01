package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
)

// =====================
// ChainKitClient related RPCs.
// =====================

// GetBestBlock makes an RPC call to chain kit client's GetBestBlock and
// asserts.
func (h *HarnessRPC) GetBestBlock(
	req *chainrpc.GetBestBlockRequest) *chainrpc.GetBestBlockResponse {

	if req == nil {
		req = &chainrpc.GetBestBlockRequest{}
	}

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.ChainKit.GetBestBlock(ctxt, req)
	h.NoError(err, "GetBestBlock")

	return resp
}

// GetBlock makes an RPC call to chain kit client's GetBlock and asserts.
func (h *HarnessRPC) GetBlock(
	req *chainrpc.GetBlockRequest) *chainrpc.GetBlockResponse {

	if req == nil {
		req = &chainrpc.GetBlockRequest{}
	}

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.ChainKit.GetBlock(ctxt, req)
	h.NoError(err, "GetBlock")

	return resp
}

// GetBlockHeader makes an RPC call to chain kit client's GetBlockHeader and
// asserts.
func (h *HarnessRPC) GetBlockHeader(
	req *chainrpc.GetBlockHeaderRequest) *chainrpc.GetBlockHeaderResponse {

	if req == nil {
		req = &chainrpc.GetBlockHeaderRequest{}
	}

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.ChainKit.GetBlockHeader(ctxt, req)
	h.NoError(err, "GetBlockHeader")

	return resp
}

// GetBlockHash makes an RPC call to chain kit client's GetBlockHash and
// asserts.
func (h *HarnessRPC) GetBlockHash(
	req *chainrpc.GetBlockHashRequest) *chainrpc.GetBlockHashResponse {

	if req == nil {
		req = &chainrpc.GetBlockHashRequest{}
	}

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.ChainKit.GetBlockHash(ctxt, req)
	h.NoError(err, "GetBlockHash")

	return resp
}
