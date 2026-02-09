package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/switchrpc"
)

// =====================
// SwitchClient related RPCs.
// =====================

// SendOnion makes a RPC call to SendOnion and returns the error.
//
//nolint:lll
func (h *HarnessRPC) SendOnion(
	req *switchrpc.SendOnionRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Switch.SendOnion(ctxt, req)

	return err
}

// TrackOnion makes a RPC call to TrackOnion and asserts.
//
//nolint:lll
func (h *HarnessRPC) TrackOnion(
	req *switchrpc.TrackOnionRequest) *switchrpc.TrackOnionResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Switch.TrackOnion(ctxt, req)
	h.NoError(err, "TrackOnion")

	return resp
}

// BuildOnion makes a RPC call to BuildOnion and asserts.
//
//nolint:lll
func (h *HarnessRPC) BuildOnion(
	req *switchrpc.BuildOnionRequest) *switchrpc.BuildOnionResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Switch.BuildOnion(ctxt, req)
	h.NoError(err, "BuildOnion")

	return resp
}
