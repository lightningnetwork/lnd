package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// =====================
// LightningClient related RPCs.
// =====================

// NewAddress makes a RPC call to NewAddress and asserts.
func (h *HarnessRPC) NewAddress(
	req *lnrpc.NewAddressRequest) *lnrpc.NewAddressResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.NewAddress(ctxt, req)
	h.NoError(err, "NewAddress")

	return resp
}

// WalletBalance makes a RPC call to WalletBalance and asserts.
func (h *HarnessRPC) WalletBalance() *lnrpc.WalletBalanceResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.WalletBalanceRequest{}
	resp, err := h.LN.WalletBalance(ctxt, req)
	h.NoError(err, "WalletBalance")

	return resp
}

// ListPeers makes a RPC call to the node's ListPeers and asserts.
func (h *HarnessRPC) ListPeers() *lnrpc.ListPeersResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.ListPeers(ctxt, &lnrpc.ListPeersRequest{})
	h.NoError(err, "ListPeers")

	return resp
}

// DisconnectPeer calls the DisconnectPeer RPC on a given node with a specified
// public key string and asserts there's no error.
func (h *HarnessRPC) DisconnectPeer(
	pubkey string) *lnrpc.DisconnectPeerResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.DisconnectPeerRequest{PubKey: pubkey}

	resp, err := h.LN.DisconnectPeer(ctxt, req)
	h.NoError(err, "DisconnectPeer")

	return resp
}

// DeleteAllPayments makes a RPC call to the node's DeleteAllPayments and
// asserts.
func (h *HarnessRPC) DeleteAllPayments() {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.DeleteAllPaymentsRequest{}
	_, err := h.LN.DeleteAllPayments(ctxt, req)
	h.NoError(err, "DeleteAllPayments")
}

// GetInfo calls the GetInfo RPC on a given node and asserts there's no error.
func (h *HarnessRPC) GetInfo() *lnrpc.GetInfoResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	info, err := h.LN.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	h.NoError(err, "GetInfo")

	return info
}

// ConnectPeer makes a RPC call to ConnectPeer and asserts there's no error.
func (h *HarnessRPC) ConnectPeer(
	req *lnrpc.ConnectPeerRequest) *lnrpc.ConnectPeerResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.ConnectPeer(ctxt, req)
	h.NoError(err, "ConnectPeer")

	return resp
}

// ListChannels list the channels for the given node and asserts it's
// successful.
func (h *HarnessRPC) ListChannels(
	req *lnrpc.ListChannelsRequest) *lnrpc.ListChannelsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.ListChannels(ctxt, req)
	h.NoError(err, "ListChannels")

	return resp
}

// PendingChannels makes a RPC request to PendingChannels and asserts there's
// no error.
func (h *HarnessRPC) PendingChannels() *lnrpc.PendingChannelsResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	resp, err := h.LN.PendingChannels(ctxt, pendingChansRequest)
	h.NoError(err, "PendingChannels")

	return resp
}

// ClosedChannels makes a RPC call to node's ClosedChannels and asserts.
func (h *HarnessRPC) ClosedChannels(
	req *lnrpc.ClosedChannelsRequest) *lnrpc.ClosedChannelsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.ClosedChannels(ctxt, req)
	h.NoError(err, "ClosedChannels")

	return resp
}

// ListPayments lists the node's payments and asserts.
func (h *HarnessRPC) ListPayments(
	req *lnrpc.ListPaymentsRequest) *lnrpc.ListPaymentsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.ListPayments(ctxt, req)
	h.NoError(err, "ListPayments")

	return resp
}

// ListInvoices list the node's invoice using the request and asserts.
func (h *HarnessRPC) ListInvoices(
	req *lnrpc.ListInvoiceRequest) *lnrpc.ListInvoiceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.ListInvoices(ctxt, req)
	h.NoError(err, "ListInvoice")

	return resp
}

// DescribeGraph makes a RPC call to the node's DescribeGraph and asserts. It
// takes a bool to indicate whether we want to include private edges or not.
func (h *HarnessRPC) DescribeGraph(
	req *lnrpc.ChannelGraphRequest) *lnrpc.ChannelGraph {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.DescribeGraph(ctxt, req)
	h.NoError(err, "DescribeGraph")

	return resp
}

// ChannelBalance gets the channel balance and asserts.
func (h *HarnessRPC) ChannelBalance() *lnrpc.ChannelBalanceResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := h.LN.ChannelBalance(ctxt, req)
	h.NoError(err, "ChannelBalance")

	return resp
}

type OpenChanClient lnrpc.Lightning_OpenChannelClient

// OpenChannel makes a rpc call to LightningClient and returns the open channel
// client.
func (h *HarnessRPC) OpenChannel(req *lnrpc.OpenChannelRequest) OpenChanClient {
	stream, err := h.LN.OpenChannel(h.runCtx, req)
	h.NoError(err, "OpenChannel")

	return stream
}

type CloseChanClient lnrpc.Lightning_CloseChannelClient

// CloseChannel makes a rpc call to LightningClient and returns the close
// channel client.
func (h *HarnessRPC) CloseChannel(
	req *lnrpc.CloseChannelRequest) CloseChanClient {

	// Use runCtx here instead of a timeout context to keep the client
	// alive for the entire test case.
	stream, err := h.LN.CloseChannel(h.runCtx, req)
	h.NoError(err, "CloseChannel")

	return stream
}

// FundingStateStep makes a RPC call to FundingStateStep and asserts.
func (h *HarnessRPC) FundingStateStep(
	msg *lnrpc.FundingTransitionMsg) *lnrpc.FundingStateStepResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.FundingStateStep(ctxt, msg)
	h.NoError(err, "FundingStateStep")

	return resp
}

// FundingStateStepAssertErr makes a RPC call to FundingStateStep and asserts
// there's an error.
func (h *HarnessRPC) FundingStateStepAssertErr(m *lnrpc.FundingTransitionMsg) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.FundingStateStep(ctxt, m)
	require.Error(h, err, "expected an error from FundingStateStep")
}

// AddInvoice adds a invoice for the given node and asserts.
func (h *HarnessRPC) AddInvoice(req *lnrpc.Invoice) *lnrpc.AddInvoiceResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	invoice, err := h.LN.AddInvoice(ctxt, req)
	h.NoError(err, "AddInvoice")

	return invoice
}

// AbandonChannel makes a RPC call to AbandonChannel and asserts.
func (h *HarnessRPC) AbandonChannel(
	req *lnrpc.AbandonChannelRequest) *lnrpc.AbandonChannelResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.AbandonChannel(ctxt, req)
	h.NoError(err, "AbandonChannel")

	return resp
}

// ExportAllChanBackups makes a RPC call to the node's ExportAllChannelBackups
// and asserts.
func (h *HarnessRPC) ExportAllChanBackups() *lnrpc.ChanBackupSnapshot {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ChanBackupExportRequest{}
	chanBackup, err := h.LN.ExportAllChannelBackups(ctxt, req)
	h.NoError(err, "ExportAllChannelBackups")

	return chanBackup
}

// ExportChanBackup makes a RPC call to the node's ExportChannelBackup
// and asserts.
func (h *HarnessRPC) ExportChanBackup(
	chanPoint *lnrpc.ChannelPoint) *lnrpc.ChannelBackup {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ExportChannelBackupRequest{
		ChanPoint: chanPoint,
	}
	chanBackup, err := h.LN.ExportChannelBackup(ctxt, req)
	h.NoError(err, "ExportChannelBackup")

	return chanBackup
}

// RestoreChanBackups makes a RPC call to the node's RestoreChannelBackups and
// asserts.
func (h *HarnessRPC) RestoreChanBackups(
	req *lnrpc.RestoreChanBackupRequest) *lnrpc.RestoreBackupResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.RestoreChannelBackups(ctxt, req)
	h.NoError(err, "RestoreChannelBackups")

	return resp
}

type AcceptorClient lnrpc.Lightning_ChannelAcceptorClient

// ChannelAcceptor makes a RPC call to the node's ChannelAcceptor and asserts.
func (h *HarnessRPC) ChannelAcceptor() (AcceptorClient, context.CancelFunc) {
	// Use runCtx here instead of a timeout context to keep the client
	// alive for the entire test case.
	ctxt, cancel := context.WithCancel(h.runCtx)
	resp, err := h.LN.ChannelAcceptor(ctxt)
	h.NoError(err, "ChannelAcceptor")

	return resp, cancel
}
