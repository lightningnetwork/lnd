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
