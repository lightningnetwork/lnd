package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/switchrpc"
)

// =====================
// SwitchClient related RPCs.
// =====================

// FetchAttemptResults makes a RPC call to FetchAttemptResults and asserts.
//
//nolint:lll
func (h *HarnessRPC) FetchAttemptResults(
	req *switchrpc.FetchAttemptResultsRequest) *switchrpc.FetchAttemptResultsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Switch.FetchAttemptResults(ctxt, req)
	h.NoError(err, "FetchAttemptResult")

	return resp
}

// DeleteAttemptResult makes a RPC call to DeleteAttemptResult and asserts.
//
//nolint:lll
func (h *HarnessRPC) DeleteAttemptResult(
	req *switchrpc.DeleteAttemptResultRequest) *switchrpc.DeleteAttemptResultResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Switch.DeleteAttemptResult(ctxt, req)
	h.NoError(err, "DeleteAttemptResult")

	return resp
}
