package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// =====================
// WalletKitClient related RPCs.
// =====================

type (
	SignReq    *walletrpc.SignMessageWithAddrResponse
	VerifyResp *walletrpc.VerifyMessageWithAddrResponse
)

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

// FundPsbtAssertErr makes a RPC call to the node's FundPsbt and asserts an
// error is returned.
func (h *HarnessRPC) FundPsbtAssertErr(req *walletrpc.FundPsbtRequest) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.WalletKit.FundPsbt(ctxt, req)
	require.Error(h, err, "expected error returned")
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

// DeriveNextKey makes a RPC call to the DeriveNextKey and asserts.
func (h *HarnessRPC) DeriveNextKey(
	req *walletrpc.KeyReq) *signrpc.KeyDescriptor {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	key, err := h.WalletKit.DeriveNextKey(ctxt, req)
	h.NoError(err, "DeriveNextKey")

	return key
}

// ListAddresses makes a RPC call to the ListAddresses and asserts.
func (h *HarnessRPC) ListAddresses(
	req *walletrpc.ListAddressesRequest) *walletrpc.ListAddressesResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	key, err := h.WalletKit.ListAddresses(ctxt, req)
	h.NoError(err, "ListAddresses")

	return key
}

// SignMessageWithAddr makes a RPC call to the SignMessageWithAddr and asserts.
func (h *HarnessRPC) SignMessageWithAddr(
	req *walletrpc.SignMessageWithAddrRequest) SignReq {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	key, err := h.WalletKit.SignMessageWithAddr(ctxt, req)
	h.NoError(err, "SignMessageWithAddr")

	return key
}

// VerifyMessageWithAddr makes a RPC call to
// the VerifyMessageWithAddr and asserts.
func (h *HarnessRPC) VerifyMessageWithAddr(
	req *walletrpc.VerifyMessageWithAddrRequest) VerifyResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	key, err := h.WalletKit.VerifyMessageWithAddr(ctxt, req)
	h.NoError(err, "VerifyMessageWithAddr")

	return key
}

// ListSweeps makes a ListSweeps RPC call to the node's WalletKit client.
func (h *HarnessRPC) ListSweeps(
	req *walletrpc.ListSweepsRequest) *walletrpc.ListSweepsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.ListSweeps(ctxt, req)
	h.NoError(err, "ListSweeps")

	return resp
}

// PendingSweeps makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessRPC) PendingSweeps() *walletrpc.PendingSweepsResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &walletrpc.PendingSweepsRequest{}

	resp, err := h.WalletKit.PendingSweeps(ctxt, req)
	h.NoError(err, "PendingSweeps")

	return resp
}

// PublishTransaction makes an RPC call to the node's WalletKitClient and
// asserts.
func (h *HarnessRPC) PublishTransaction(
	req *walletrpc.Transaction) *walletrpc.PublishResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.PublishTransaction(ctxt, req)
	h.NoError(err, "PublishTransaction")

	return resp
}

// GetTransaction makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessRPC) GetTransaction(
	req *walletrpc.GetTransactionRequest) *lnrpc.Transaction {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.GetTransaction(ctxt, req)
	h.NoError(err, "GetTransaction")

	return resp
}

// RemoveTransaction makes an RPC call to the node's WalletKitClient and
// asserts.
//
//nolint:ll
func (h *HarnessRPC) RemoveTransaction(
	req *walletrpc.GetTransactionRequest) *walletrpc.RemoveTransactionResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.RemoveTransaction(ctxt, req)
	h.NoError(err, "RemoveTransaction")

	return resp
}

// BumpFee makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessRPC) BumpFee(
	req *walletrpc.BumpFeeRequest) *walletrpc.BumpFeeResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.BumpFee(ctxt, req)
	h.NoError(err, "BumpFee")

	return resp
}

// BumpFeeAssertErr makes a RPC call to the node's WalletKitClient and asserts
// that an error is returned.
func (h *HarnessRPC) BumpFeeAssertErr(req *walletrpc.BumpFeeRequest) error {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.WalletKit.BumpFee(ctxt, req)
	require.Errorf(h, err, "%s: expect BumpFee to return an error", h.Name)

	return err
}

// BumpForceCloseFee makes a RPC call to the node's WalletKitClient and asserts.
//
//nolint:ll
func (h *HarnessRPC) BumpForceCloseFee(
	req *walletrpc.BumpForceCloseFeeRequest) *walletrpc.BumpForceCloseFeeResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.BumpForceCloseFee(ctxt, req)
	h.NoError(err, "BumpForceCloseFee")

	return resp
}

// ListAccounts makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessRPC) ListAccounts(
	req *walletrpc.ListAccountsRequest) *walletrpc.ListAccountsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.ListAccounts(ctxt, req)
	h.NoError(err, "ListAccounts")

	return resp
}

// ImportAccount makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessRPC) ImportAccount(
	req *walletrpc.ImportAccountRequest) *walletrpc.ImportAccountResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.ImportAccount(ctxt, req)
	h.NoError(err, "ImportAccount")

	return resp
}

// ImportAccountAssertErr makes the ImportAccount RPC call and asserts an error
// is returned. It then returns the error.
func (h *HarnessRPC) ImportAccountAssertErr(
	req *walletrpc.ImportAccountRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.WalletKit.ImportAccount(ctxt, req)
	require.Error(h, err)

	return err
}

// ImportPublicKey makes a RPC call to the node's WalletKitClient and asserts.
//
//nolint:ll
func (h *HarnessRPC) ImportPublicKey(
	req *walletrpc.ImportPublicKeyRequest) *walletrpc.ImportPublicKeyResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.ImportPublicKey(ctxt, req)
	h.NoError(err, "ImportPublicKey")

	return resp
}

// SignPsbt makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessRPC) SignPsbt(
	req *walletrpc.SignPsbtRequest) *walletrpc.SignPsbtResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.SignPsbt(ctxt, req)
	h.NoError(err, "SignPsbt")

	return resp
}

// SignPsbtErr makes a RPC call to the node's WalletKitClient and asserts
// an error returned.
func (h *HarnessRPC) SignPsbtErr(req *walletrpc.SignPsbtRequest) error {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.WalletKit.SignPsbt(ctxt, req)
	require.Errorf(h, err, "%s: expect sign psbt to return an error",
		h.Name)

	return err
}

// ImportTapscript makes a RPC call to the node's WalletKitClient and asserts.
//
//nolint:ll
func (h *HarnessRPC) ImportTapscript(
	req *walletrpc.ImportTapscriptRequest) *walletrpc.ImportTapscriptResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.ImportTapscript(ctxt, req)
	h.NoError(err, "ImportTapscript")

	return resp
}

// RequiredReserve makes a RPC call to the node's WalletKitClient and asserts.
//
//nolint:ll
func (h *HarnessRPC) RequiredReserve(
	req *walletrpc.RequiredReserveRequest) *walletrpc.RequiredReserveResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.RequiredReserve(ctxt, req)
	h.NoError(err, "RequiredReserve")

	return resp
}

// ListLeases makes a ListLeases RPC call to the node's WalletKit client.
func (h *HarnessRPC) ListLeases() *walletrpc.ListLeasesResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.WalletKit.ListLeases(
		ctxt, &walletrpc.ListLeasesRequest{},
	)
	h.NoError(err, "ListLeases")

	return resp
}
