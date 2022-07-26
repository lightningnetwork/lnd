package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
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

// DeriveKey makes a RPC call to the DeriveKey and asserts.
func (h *HarnessRPC) DeriveKey(kl *signrpc.KeyLocator) *signrpc.KeyDescriptor {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	key, err := h.WalletKit.DeriveKey(ctxt, kl)
	h.NoError(err, "DeriveKey")

	return key
}

// SendOutputs makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessRPC) SendOutputs(
	req *walletrpc.SendOutputsRequest) *walletrpc.SendOutputsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.SendOutputs(ctxt, req)
	h.NoError(err, "SendOutputs")

	return resp
}
