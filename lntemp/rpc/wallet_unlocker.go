package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// =====================
// WalletUnlockerClient related RPCs.
// =====================

// UnlockWallet makes a RPC request to WalletUnlocker and asserts there's no
// error.
func (h *HarnessRPC) UnlockWallet(
	req *lnrpc.UnlockWalletRequest) *lnrpc.UnlockWalletResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletUnlocker.UnlockWallet(ctxt, req)
	h.NoError(err, "UnlockWallet")

	return resp
}

// InitWallet makes a RPC request to WalletUnlocker and asserts there's no
// error.
func (h *HarnessRPC) InitWallet(
	req *lnrpc.InitWalletRequest) *lnrpc.InitWalletResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletUnlocker.InitWallet(ctxt, req)
	h.NoError(err, "InitWallet")

	return resp
}

// GenSeed makes a RPC request to WalletUnlocker and asserts there's no error.
func (h *HarnessRPC) GenSeed(req *lnrpc.GenSeedRequest) *lnrpc.GenSeedResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletUnlocker.GenSeed(ctxt, req)
	h.NoError(err, "GenSeed")

	return resp
}

// ChangePassword makes a RPC request to WalletUnlocker and asserts there's no
// error.
func (h *HarnessRPC) ChangePassword(
	req *lnrpc.ChangePasswordRequest) *lnrpc.ChangePasswordResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletUnlocker.ChangePassword(ctxt, req)
	h.NoError(err, "ChangePassword")

	return resp
}
