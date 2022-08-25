package rpc

import (
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
)

// =====================
// ChainClient related RPCs.
// =====================

type ConfNtfnClient chainrpc.ChainNotifier_RegisterConfirmationsNtfnClient

// RegisterConfirmationsNtfn creates a notification client to watch a given
// transaction being confirmed.
func (h *HarnessRPC) RegisterConfirmationsNtfn(
	req *chainrpc.ConfRequest) ConfNtfnClient {

	// RegisterConfirmationsNtfn needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	client, err := h.ChainClient.RegisterConfirmationsNtfn(
		h.runCtx, req,
	)
	h.NoError(err, "RegisterConfirmationsNtfn")

	return client
}

type SpendClient chainrpc.ChainNotifier_RegisterSpendNtfnClient

// RegisterSpendNtfn creates a notification client to watch a given
// transaction being spent.
func (h *HarnessRPC) RegisterSpendNtfn(req *chainrpc.SpendRequest) SpendClient {
	// RegisterSpendNtfn needs to have the context alive for the entire
	// test case as the returned client will be used for send and receive
	// events stream. Thus we use runCtx here instead of a timeout context.
	client, err := h.ChainClient.RegisterSpendNtfn(
		h.runCtx, req,
	)
	h.NoError(err, "RegisterSpendNtfn")

	return client
}
