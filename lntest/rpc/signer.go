package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/stretchr/testify/require"
)

// =====================
// Signer related RPCs.
// =====================

// DeriveSharedKey makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessRPC) DeriveSharedKey(
	req *signrpc.SharedKeyRequest) *signrpc.SharedKeyResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.DeriveSharedKey(ctxt, req)
	h.NoError(err, "DeriveSharedKey")

	return resp
}

// DeriveSharedKeyErr makes a RPC call to the node's SignerClient and asserts
// there is an error.
func (h *HarnessRPC) DeriveSharedKeyErr(req *signrpc.SharedKeyRequest) error {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Signer.DeriveSharedKey(ctxt, req)
	require.Error(h, err, "expected error from calling DeriveSharedKey")

	return err
}

// SignOutputRaw makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessRPC) SignOutputRaw(req *signrpc.SignReq) *signrpc.SignResp {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.SignOutputRaw(ctxt, req)
	h.NoError(err, "SignOutputRaw")

	return resp
}

// SignOutputRawErr makes a RPC call to the node's SignerClient and asserts an
// error is returned.
func (h *HarnessRPC) SignOutputRawErr(req *signrpc.SignReq) error {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Signer.SignOutputRaw(ctxt, req)
	require.Error(h, err, "expect to fail to sign raw output")

	return err
}

// MuSig2CreateSession makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessRPC) MuSig2CreateSession(
	req *signrpc.MuSig2SessionRequest) *signrpc.MuSig2SessionResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.MuSig2CreateSession(ctxt, req)
	h.NoError(err, "MuSig2CreateSession")

	return resp
}

// MuSig2CreateSessionErr makes an RPC call to the node's SignerClient and
// asserts an error is returned.
func (h *HarnessRPC) MuSig2CreateSessionErr(
	req *signrpc.MuSig2SessionRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Signer.MuSig2CreateSession(ctxt, req)
	require.Error(h, err, "expected error from calling MuSig2CreateSession")

	return err
}

// MuSig2CombineKeys makes a RPC call to the node's SignerClient and asserts.
//
//nolint:ll
func (h *HarnessRPC) MuSig2CombineKeys(
	req *signrpc.MuSig2CombineKeysRequest) *signrpc.MuSig2CombineKeysResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.MuSig2CombineKeys(ctxt, req)
	h.NoError(err, "MuSig2CombineKeys")

	return resp
}

// MuSig2CombineKeysErr makes an RPC call to the node's SignerClient and
// asserts an error is returned.
func (h *HarnessRPC) MuSig2CombineKeysErr(
	req *signrpc.MuSig2CombineKeysRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Signer.MuSig2CombineKeys(ctxt, req)
	require.Error(h, err, "expected error from calling MuSig2CombineKeys")

	return err
}

// MuSig2RegisterNonces makes a RPC call to the node's SignerClient and asserts.
//
//nolint:ll
func (h *HarnessRPC) MuSig2RegisterNonces(
	req *signrpc.MuSig2RegisterNoncesRequest) *signrpc.MuSig2RegisterNoncesResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.MuSig2RegisterNonces(ctxt, req)
	h.NoError(err, "MuSig2RegisterNonces")

	return resp
}

// MuSig2RegisterNoncesErr makes a RPC call to the node's SignerClient and
// asserts an error is returned.
func (h *HarnessRPC) MuSig2RegisterNoncesErr(
	req *signrpc.MuSig2RegisterNoncesRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Signer.MuSig2RegisterNonces(ctxt, req)
	require.Error(h, err, "expected error from MuSig2RegisterNonces")

	return err
}

// MuSig2RegisterCombinedNonce makes a RPC call to the node's SignerClient and
// asserts.
//
//nolint:ll
func (h *HarnessRPC) MuSig2RegisterCombinedNonce(
	req *signrpc.MuSig2RegisterCombinedNonceRequest) *signrpc.MuSig2RegisterCombinedNonceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.MuSig2RegisterCombinedNonce(ctxt, req)
	h.NoError(err, "MuSig2RegisterCombinedNonce")

	return resp
}

// MuSig2RegisterCombinedNonceErr makes a RPC call to the node's SignerClient
// and asserts an error is returned.
func (h *HarnessRPC) MuSig2RegisterCombinedNonceErr(
	req *signrpc.MuSig2RegisterCombinedNonceRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Signer.MuSig2RegisterCombinedNonce(ctxt, req)
	require.Error(h, err, "expected error from MuSig2RegisterCombinedNonce")

	return err
}

// MuSig2GetCombinedNonce makes a RPC call to the node's SignerClient and
// asserts.
//
//nolint:ll
func (h *HarnessRPC) MuSig2GetCombinedNonce(
	req *signrpc.MuSig2GetCombinedNonceRequest) *signrpc.MuSig2GetCombinedNonceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.MuSig2GetCombinedNonce(ctxt, req)
	h.NoError(err, "MuSig2GetCombinedNonce")

	return resp
}

// MuSig2GetCombinedNonceErr makes a RPC call to the node's SignerClient and
// asserts an error is returned.
func (h *HarnessRPC) MuSig2GetCombinedNonceErr(
	req *signrpc.MuSig2GetCombinedNonceRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Signer.MuSig2GetCombinedNonce(ctxt, req)
	require.Error(h, err, "expected error from MuSig2GetCombinedNonce")

	return err
}

// MuSig2Sign makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessRPC) MuSig2Sign(
	req *signrpc.MuSig2SignRequest) *signrpc.MuSig2SignResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.MuSig2Sign(ctxt, req)
	h.NoError(err, "MuSig2Sign")

	return resp
}

// MuSig2SignErr makes a RPC call to the node's SignerClient and asserts an
// error is returned.
func (h *HarnessRPC) MuSig2SignErr(req *signrpc.MuSig2SignRequest) error {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Signer.MuSig2Sign(ctxt, req)
	require.Error(h, err, "expect an error")

	return err
}

// MuSig2CombineSig makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessRPC) MuSig2CombineSig(
	r *signrpc.MuSig2CombineSigRequest) *signrpc.MuSig2CombineSigResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.MuSig2CombineSig(ctxt, r)
	h.NoError(err, "MuSig2CombineSig")

	return resp
}

// MuSig2Cleanup makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessRPC) MuSig2Cleanup(
	req *signrpc.MuSig2CleanupRequest) *signrpc.MuSig2CleanupResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.MuSig2Cleanup(ctxt, req)
	h.NoError(err, "MuSig2Cleanup")

	return resp
}

// SignMessageSigner makes a RPC call to the node's SignerClient and asserts.
//
// NOTE: there's already `SignMessage` in `h.LN`.
func (h *HarnessRPC) SignMessageSigner(
	req *signrpc.SignMessageReq) *signrpc.SignMessageResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.SignMessage(ctxt, req)
	h.NoError(err, "SignMessage")

	return resp
}

// VerifyMessageSigner makes a RPC call to the node's SignerClient and asserts.
//
// NOTE: there's already `VerifyMessageSigner` in `h.LN`.
func (h *HarnessRPC) VerifyMessageSigner(
	req *signrpc.VerifyMessageReq) *signrpc.VerifyMessageResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.VerifyMessage(ctxt, req)
	h.NoError(err, "VerifyMessage")

	return resp
}

// ComputeInputScript makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessRPC) ComputeInputScript(
	req *signrpc.SignReq) *signrpc.InputScriptResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Signer.ComputeInputScript(ctxt, req)
	h.NoError(err, "ComputeInputScript")

	return resp
}
