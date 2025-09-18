package rpc

import (
	"context"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
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

	req := &lnrpc.DeleteAllPaymentsRequest{AllPayments: true}
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

// ConnectPeerAssertErr makes a RPC call to ConnectPeer and asserts an error
// returned.
func (h *HarnessRPC) ConnectPeerAssertErr(req *lnrpc.ConnectPeerRequest) error {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.ConnectPeer(ctxt, req)
	require.Error(h, err, "expected an error from ConnectPeer")

	return err
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

	pendingChansRequest := &lnrpc.PendingChannelsRequest{
		IncludeRawTx: true,
	}
	resp, err := h.LN.PendingChannels(ctxt, pendingChansRequest)

	// TODO(yy): We may get a `unable to find arbitrator` error from the
	// rpc point, due to a timing issue in rpcserver,
	// 1. `r.server.chanStateDB.FetchClosedChannels` fetches
	//    the pending force close channel.
	// 2. `r.arbitratorPopulateForceCloseResp` relies on the
	//    channel arbitrator to get the report, and,
	// 3. the arbitrator may be deleted due to the force close
	//    channel being resolved.
	// Somewhere along the line is missing a lock to keep the data
	// consistent.
	//
	// Return if there's no error.
	if err == nil {
		return resp
	}

	// Otherwise, give it a second shot if it's the arbitrator error.
	if strings.Contains(err.Error(), "unable to find arbitrator") {
		resp, err = h.LN.PendingChannels(ctxt, pendingChansRequest)
	}

	// It's very unlikely we'd get the arbitrator not found error again.
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

	if req == nil {
		req = &lnrpc.ListInvoiceRequest{}
	}

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

// AddInvoiceAssertErr makes a RPC call to AddInvoice and asserts an error
// has returned with a specific error message.
func (h *HarnessRPC) AddInvoiceAssertErr(req *lnrpc.Invoice, errStr string) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.AddInvoice(ctxt, req)
	require.ErrorContains(h, err, errStr)
}

// GetChanInfoAssertErr makes an RPC call to GetChanInfo and asserts an error
// has returned with a specific error message.
func (h *HarnessRPC) GetChanInfoAssertErr(req *lnrpc.ChanInfoRequest,
	errStr string) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.GetChanInfo(ctxt, req)
	require.ErrorContains(h, err, errStr)
}

// GetNodeInfoAssertErr makes an RPC call to GetNodeInfo and asserts an error
// has returned with a specific error message.
func (h *HarnessRPC) GetNodeInfoAssertErr(req *lnrpc.NodeInfoRequest,
	errStr string) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.GetNodeInfo(ctxt, req)
	require.ErrorContains(h, err, errStr)
}

// SendCustomMessageAssertErr makes an RPC call to SendCustomMessage and asserts
// an error has returned with a specific error message.
func (h *HarnessRPC) SendCustomMessageAssertErr(
	req *lnrpc.SendCustomMessageRequest, errStr string) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.SendCustomMessage(ctxt, req)
	require.ErrorContains(h, err, errStr)
}

// LookupInvoiceAssertErr makes an RPC call to LookupInvoice and asserts an
// error has returned with a specific error message.
func (h *HarnessRPC) LookupInvoiceAssertErr(req *lnrpc.PaymentHash,
	errStr string) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.LookupInvoice(ctxt, req)
	require.ErrorContains(h, err, errStr)
}

// LookupHTLCResolutionAssertErr makes an RPC call to LookupHTLCResolution and
// asserts an error has returned with a specific error message.
func (h *HarnessRPC) LookupHTLCResolutionAssertErr(
	req *lnrpc.LookupHtlcResolutionRequest, errStr string) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.LookupHtlcResolution(ctxt, req)
	require.ErrorContains(h, err, errStr)
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

// SendCoins sends a given amount of money to the specified address from the
// passed node.
func (h *HarnessRPC) SendCoins(
	req *lnrpc.SendCoinsRequest) *lnrpc.SendCoinsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.SendCoins(ctxt, req)
	h.NoError(err, "SendCoins")

	return resp
}

// SendCoinsAssertErr sends a given amount of money to the specified address
// from the passed node and asserts an error has returned.
func (h *HarnessRPC) SendCoinsAssertErr(req *lnrpc.SendCoinsRequest) error {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.SendCoins(ctxt, req)
	require.Error(h, err, "node %s didn't not return an error", h.Name)

	return err
}

// GetTransactions makes a RPC call to GetTransactions and asserts.
func (h *HarnessRPC) GetTransactions(
	req *lnrpc.GetTransactionsRequest) *lnrpc.TransactionDetails {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	if req == nil {
		req = &lnrpc.GetTransactionsRequest{}
	}

	resp, err := h.LN.GetTransactions(ctxt, req)
	h.NoError(err, "GetTransactions")

	return resp
}

// SignMessage makes a RPC call to node's SignMessage and asserts.
func (h *HarnessRPC) SignMessage(msg []byte) *lnrpc.SignMessageResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.SignMessageRequest{Msg: msg}
	resp, err := h.LN.SignMessage(ctxt, req)
	h.NoError(err, "SignMessage")

	return resp
}

// VerifyMessage makes a RPC call to node's VerifyMessage and asserts.
func (h *HarnessRPC) VerifyMessage(msg []byte,
	sig string) *lnrpc.VerifyMessageResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.VerifyMessageRequest{Msg: msg, Signature: sig}
	resp, err := h.LN.VerifyMessage(ctxt, req)
	h.NoError(err, "VerifyMessage")

	return resp
}

// GetRecoveryInfo uses the specified node to make a RPC call to
// GetRecoveryInfo and asserts.
func (h *HarnessRPC) GetRecoveryInfo(
	req *lnrpc.GetRecoveryInfoRequest) *lnrpc.GetRecoveryInfoResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	if req == nil {
		req = &lnrpc.GetRecoveryInfoRequest{}
	}

	resp, err := h.LN.GetRecoveryInfo(ctxt, req)
	h.NoError(err, "GetRecoveryInfo")

	return resp
}

// BatchOpenChannel makes a RPC call to BatchOpenChannel and asserts.
func (h *HarnessRPC) BatchOpenChannel(
	req *lnrpc.BatchOpenChannelRequest) *lnrpc.BatchOpenChannelResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.BatchOpenChannel(ctxt, req)
	h.NoError(err, "BatchOpenChannel")

	return resp
}

// BatchOpenChannelAssertErr makes a RPC call to BatchOpenChannel and asserts
// there's an error returned.
func (h *HarnessRPC) BatchOpenChannelAssertErr(
	req *lnrpc.BatchOpenChannelRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.BatchOpenChannel(ctxt, req)
	require.Error(h, err, "expecte batch open channel fail")

	return err
}

// QueryRoutes makes a RPC call to QueryRoutes and asserts.
func (h *HarnessRPC) QueryRoutes(
	req *lnrpc.QueryRoutesRequest) *lnrpc.QueryRoutesResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	routes, err := h.LN.QueryRoutes(ctxt, req)
	h.NoError(err, "QueryRoutes")

	return routes
}

type SendToRouteClient lnrpc.Lightning_SendToRouteClient

// SendToRoute makes a RPC call to SendToRoute and asserts.
func (h *HarnessRPC) SendToRoute() SendToRouteClient {
	// SendToRoute needs to have the context alive for the entire test case
	// as the returned client will be used for send and receive payment
	// stream. Thus we use runCtx here instead of a timeout context.
	client, err := h.LN.SendToRoute(h.runCtx)
	h.NoError(err, "SendToRoute")

	return client
}

// SendToRouteSync makes a RPC call to SendToRouteSync and asserts.
func (h *HarnessRPC) SendToRouteSync(
	req *lnrpc.SendToRouteRequest) *lnrpc.SendResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.SendToRouteSync(ctxt, req)
	h.NoError(err, "SendToRouteSync")

	return resp
}

// UpdateChannelPolicy makes a RPC call to UpdateChannelPolicy and asserts.
func (h *HarnessRPC) UpdateChannelPolicy(
	req *lnrpc.PolicyUpdateRequest) *lnrpc.PolicyUpdateResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.UpdateChannelPolicy(ctxt, req)
	h.NoError(err, "UpdateChannelPolicy")

	return resp
}

type InvoiceUpdateClient lnrpc.Lightning_SubscribeInvoicesClient

// SubscribeInvoices creates a subscription client for invoice events and
// asserts its creation.
//
// NOTE: make sure to subscribe an invoice as early as possible as it takes
// some time for the lnd to create the subscription client. If an invoice is
// added right after the subscription, it may be missed. However, if AddIndex
// or SettleIndex is used in the request, it will be fine as a backlog will
// always be sent.
func (h *HarnessRPC) SubscribeInvoices(
	req *lnrpc.InvoiceSubscription) InvoiceUpdateClient {

	// SubscribeInvoices needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	client, err := h.LN.SubscribeInvoices(h.runCtx, req)
	h.NoError(err, "SubscribeInvoices")

	return client
}

type BackupSubscriber lnrpc.Lightning_SubscribeChannelBackupsClient

// SubscribeChannelBackups creates a client to listen to channel backup stream.
func (h *HarnessRPC) SubscribeChannelBackups() BackupSubscriber {
	// Use runCtx here instead of timeout context to keep the stream client
	// alive.
	backupStream, err := h.LN.SubscribeChannelBackups(
		h.runCtx, &lnrpc.ChannelBackupSubscription{},
	)
	h.NoError(err, "SubscribeChannelBackups")

	return backupStream
}

// VerifyChanBackup makes a RPC call to node's VerifyChanBackup and asserts.
func (h *HarnessRPC) VerifyChanBackup(
	ss *lnrpc.ChanBackupSnapshot) *lnrpc.VerifyChanBackupResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.VerifyChanBackup(ctxt, ss)
	h.NoError(err, "VerifyChanBackup")

	return resp
}

// LookupInvoice queries the node's invoices using the specified rHash.
func (h *HarnessRPC) LookupInvoice(rHash []byte) *lnrpc.Invoice {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	payHash := &lnrpc.PaymentHash{RHash: rHash}
	resp, err := h.LN.LookupInvoice(ctxt, payHash)
	h.NoError(err, "LookupInvoice")

	return resp
}

// DecodePayReq makes a RPC call to node's DecodePayReq and asserts.
func (h *HarnessRPC) DecodePayReq(req string) *lnrpc.PayReq {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	payReq := &lnrpc.PayReqString{PayReq: req}
	resp, err := h.LN.DecodePayReq(ctxt, payReq)
	h.NoError(err, "DecodePayReq")

	return resp
}

// ForwardingHistory makes a RPC call to the node's ForwardingHistory and
// asserts.
func (h *HarnessRPC) ForwardingHistory(
	req *lnrpc.ForwardingHistoryRequest) *lnrpc.ForwardingHistoryResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	if req == nil {
		req = &lnrpc.ForwardingHistoryRequest{}
	}

	resp, err := h.LN.ForwardingHistory(ctxt, req)
	h.NoError(err, "ForwardingHistory")

	return resp
}

type MiddlewareClient lnrpc.Lightning_RegisterRPCMiddlewareClient

// RegisterRPCMiddleware makes a RPC call to the node's RegisterRPCMiddleware
// and asserts. It also returns a cancel context which can cancel the context
// used by the client.
func (h *HarnessRPC) RegisterRPCMiddleware() (MiddlewareClient,
	context.CancelFunc) {

	ctxt, cancel := context.WithCancel(h.runCtx)

	stream, err := h.LN.RegisterRPCMiddleware(ctxt)
	h.NoError(err, "RegisterRPCMiddleware")

	return stream, cancel
}

type ChannelEventsClient lnrpc.Lightning_SubscribeChannelEventsClient

// SubscribeChannelEvents creates a subscription client for channel events and
// asserts its creation.
func (h *HarnessRPC) SubscribeChannelEvents() ChannelEventsClient {
	req := &lnrpc.ChannelEventSubscription{}

	// SubscribeChannelEvents needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	client, err := h.LN.SubscribeChannelEvents(h.runCtx, req)
	h.NoError(err, "SubscribeChannelEvents")

	return client
}

type CustomMessageClient lnrpc.Lightning_SubscribeCustomMessagesClient

type OnionMessageClient lnrpc.Lightning_SubscribeOnionMessagesClient

// SubscribeCustomMessages creates a subscription client for custom messages.
func (h *HarnessRPC) SubscribeCustomMessages() (CustomMessageClient,
	context.CancelFunc) {

	ctxt, cancel := context.WithCancel(h.runCtx)

	req := &lnrpc.SubscribeCustomMessagesRequest{}

	// SubscribeCustomMessages needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	stream, err := h.LN.SubscribeCustomMessages(ctxt, req)
	h.NoError(err, "SubscribeCustomMessages")

	return stream, cancel
}

// SendCustomMessage makes a RPC call to the node's SendCustomMessage and
// returns the response.
func (h *HarnessRPC) SendCustomMessage(
	req *lnrpc.SendCustomMessageRequest) *lnrpc.SendCustomMessageResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.SendCustomMessage(ctxt, req)
	h.NoError(err, "SendCustomMessage")

	return resp
}

// SendOnionMessage makes a RPC call to the node's SendOnionMessage and
// returns the response.
func (h *HarnessRPC) SendOnionMessage(
	req *lnrpc.SendOnionMessageRequest) *lnrpc.SendOnionMessageResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.SendOnionMessage(ctxt, req)
	h.NoError(err, "SendOnionMessage")

	return resp
}

// SubscribeOnionMessages creates a subscription client for onion messages.
func (h *HarnessRPC) SubscribeOnionMessages() (OnionMessageClient,
	context.CancelFunc) {

	ctxt, cancel := context.WithCancel(h.runCtx)

	req := &lnrpc.SubscribeOnionMessagesRequest{}

	// SubscribeCustomMessages needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	stream, err := h.LN.SubscribeOnionMessages(ctxt, req)
	h.NoError(err, "SubscribeOnionMessages")

	return stream, cancel
}

// GetChanInfo makes a RPC call to the node's GetChanInfo and returns the
// response.
func (h *HarnessRPC) GetChanInfo(
	req *lnrpc.ChanInfoRequest) *lnrpc.ChannelEdge {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.GetChanInfo(ctxt, req)
	h.NoError(err, "GetChanInfo")

	return resp
}

// LookupHtlcResolution makes a RPC call to the node's LookupHtlcResolution and
// returns the response.
//
//nolint:ll
func (h *HarnessRPC) LookupHtlcResolution(
	req *lnrpc.LookupHtlcResolutionRequest) *lnrpc.LookupHtlcResolutionResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.LookupHtlcResolution(ctxt, req)
	h.NoError(err, "LookupHtlcResolution")

	return resp
}

// LookupHtlcResolutionAssertErr makes a RPC call to the node's
// LookupHtlcResolution and asserts an RPC error is returned.
func (h *HarnessRPC) LookupHtlcResolutionAssertErr(
	req *lnrpc.LookupHtlcResolutionRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.LookupHtlcResolution(ctxt, req)
	require.Error(h, err, "expected an error")

	return err
}

// Quiesce makes an RPC call to the node's Quiesce method and returns the
// response.
func (h *HarnessRPC) Quiesce(
	req *devrpc.QuiescenceRequest) *devrpc.QuiescenceResponse {

	ctx, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	res, err := h.Dev.Quiesce(ctx, req)
	h.NoError(err, "Quiesce returned an error")

	return res
}

type PeerEventsClient lnrpc.Lightning_SubscribePeerEventsClient

// SubscribePeerEvents makes a RPC call to the node's SubscribePeerEvents and
// returns the stream client.
func (h *HarnessRPC) SubscribePeerEvents(
	req *lnrpc.PeerEventSubscription) PeerEventsClient {

	// SubscribePeerEvents needs to have the context alive for the entire
	// test case as the returned client will be used for send and receive
	// events stream. Thus we use runCtx here instead of a timeout context.
	resp, err := h.LN.SubscribePeerEvents(h.runCtx, req)
	h.NoError(err, "SubscribePeerEvents")

	return resp
}

// DeleteCanceledInvoice makes a RPC call to the node's DeleteCanceledInvoice
// and asserts.
func (h *HarnessRPC) DeleteCanceledInvoice(
	req *lnrpc.DelCanceledInvoiceReq) *lnrpc.DelCanceledInvoiceResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.LN.DeleteCanceledInvoice(ctxt, req)
	h.NoError(err, "DeleteCanceledInvoice")

	return resp
}

// DeleteCanceledInvoiceAssertErr makes a RPC call to the node's
// DeleteCanceledInvoice and asserts if an RPC error is returned.
func (h *HarnessRPC) DeleteCanceledInvoiceAssertErr(
	req *lnrpc.DelCanceledInvoiceReq, errStr string) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.LN.DeleteCanceledInvoice(ctxt, req)
	require.ErrorContains(h, err, errStr)
}
