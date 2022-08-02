package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
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

// FundPsbt makes a RPC call to node's FundPsbt and asserts.
func (h *HarnessRPC) FundPsbt(
	req *walletrpc.FundPsbtRequest) *walletrpc.FundPsbtResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.FundPsbt(ctxt, req)
	h.NoError(err, "FundPsbt")

	return resp
}

// FinalizePsbt makes a RPC call to node's FinalizePsbt and asserts.
func (h *HarnessRPC) FinalizePsbt(
	req *walletrpc.FinalizePsbtRequest) *walletrpc.FinalizePsbtResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.FinalizePsbt(ctxt, req)
	h.NoError(err, "FinalizePsbt")

	return resp
}

// LabelTransactionAssertErr makes a RPC call to the node's LabelTransaction
// and asserts an error is returned. It then returns the error.
func (h *HarnessRPC) LabelTransactionAssertErr(
	req *walletrpc.LabelTransactionRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.WalletKit.LabelTransaction(ctxt, req)
	require.Error(h, err, "expected error returned")

	return err
}

// LabelTransaction makes a RPC call to the node's LabelTransaction
// and asserts no error is returned.
func (h *HarnessRPC) LabelTransaction(req *walletrpc.LabelTransactionRequest) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.WalletKit.LabelTransaction(ctxt, req)
	h.NoError(err, "LabelTransaction")
}
