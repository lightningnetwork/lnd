package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/stretchr/testify/require"
)

// =====================
// RouterClient related RPCs.
// =====================

// UpdateChanStatus makes a UpdateChanStatus RPC call to node's RouterClient
// and asserts.
//
//nolint:ll
func (h *HarnessRPC) UpdateChanStatus(
	req *routerrpc.UpdateChanStatusRequest) *routerrpc.UpdateChanStatusResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Router.UpdateChanStatus(ctxt, req)
	h.NoError(err, "UpdateChanStatus")

	return resp
}

type PaymentClient routerrpc.Router_SendPaymentV2Client

// SendPayment sends a payment using the given node and payment request. It
// also asserts the payment being sent successfully.
func (h *HarnessRPC) SendPayment(
	req *routerrpc.SendPaymentRequest) PaymentClient {

	// SendPayment needs to have the context alive for the entire test case
	// as the router relies on the context to propagate HTLCs. Thus, we use
	// runCtx here instead of a timeout context.
	stream, err := h.Router.SendPaymentV2(h.runCtx, req)
	h.NoError(err, "SendPaymentV2")

	return stream
}

// SendPaymentWithContext sends a payment using the given node and payment
// request and does so with the passed in context.
func (h *HarnessRPC) SendPaymentWithContext(context context.Context,
	req *routerrpc.SendPaymentRequest) PaymentClient {

	require.NotNil(h.T, context, "context must not be nil")

	// SendPayment needs to have the context alive for the entire test case
	// as the router relies on the context to propagate HTLCs.
	stream, err := h.Router.SendPaymentV2(context, req)
	h.NoError(err, "SendPaymentV2")

	return stream
}

type HtlcEventsClient routerrpc.Router_SubscribeHtlcEventsClient

// SubscribeHtlcEvents makes a subscription to the HTLC events and returns a
// htlc event client.
func (h *HarnessRPC) SubscribeHtlcEvents() HtlcEventsClient {
	// Use runCtx here to keep the client alive for the scope of the test.
	client, err := h.Router.SubscribeHtlcEvents(
		h.runCtx, &routerrpc.SubscribeHtlcEventsRequest{},
	)
	h.NoError(err, "SubscribeHtlcEvents")

	return client
}

// GetMissionControlConfig makes a RPC call to the node's
// GetMissionControlConfig and asserts.
//
//nolint:ll
func (h *HarnessRPC) GetMissionControlConfig() *routerrpc.GetMissionControlConfigResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &routerrpc.GetMissionControlConfigRequest{}
	resp, err := h.Router.GetMissionControlConfig(ctxt, req)
	h.NoError(err, "GetMissionControlConfig")

	return resp
}

// SetMissionControlConfig makes a RPC call to the node's
// SetMissionControlConfig and asserts.
func (h *HarnessRPC) SetMissionControlConfig(
	config *routerrpc.MissionControlConfig) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &routerrpc.SetMissionControlConfigRequest{Config: config}
	_, err := h.Router.SetMissionControlConfig(ctxt, req)
	h.NoError(err, "SetMissionControlConfig")
}

// SetMissionControlConfigAssertErr makes a RPC call to the node's
// SetMissionControlConfig and asserts that we error.
func (h *HarnessRPC) SetMissionControlConfigAssertErr(
	config *routerrpc.MissionControlConfig) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &routerrpc.SetMissionControlConfigRequest{Config: config}
	_, err := h.Router.SetMissionControlConfig(ctxt, req)
	require.Error(h, err, "expect an error from setting import mission "+
		"control")
}

// ResetMissionControl makes a RPC call to the node's ResetMissionControl and
// asserts.
func (h *HarnessRPC) ResetMissionControl() {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &routerrpc.ResetMissionControlRequest{}
	_, err := h.Router.ResetMissionControl(ctxt, req)
	h.NoError(err, "ResetMissionControl")
}

// SendToRouteV2 makes a RPC call to SendToRouteV2 and asserts.
func (h *HarnessRPC) SendToRouteV2(
	req *routerrpc.SendToRouteRequest) *lnrpc.HTLCAttempt {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Router.SendToRouteV2(ctxt, req)
	h.NoError(err, "SendToRouteV2")

	return resp
}

// QueryProbability makes a RPC call to the node's QueryProbability and
// asserts.
//
//nolint:ll
func (h *HarnessRPC) QueryProbability(
	req *routerrpc.QueryProbabilityRequest) *routerrpc.QueryProbabilityResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Router.QueryProbability(ctxt, req)
	h.NoError(err, "QueryProbability")

	return resp
}

// XImportMissionControl makes a RPC call to the node's XImportMissionControl
// and asserts.
func (h *HarnessRPC) XImportMissionControl(
	req *routerrpc.XImportMissionControlRequest) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Router.XImportMissionControl(ctxt, req)
	h.NoError(err, "XImportMissionControl")
}

// XImportMissionControlAssertErr makes a RPC call to the node's
// XImportMissionControl
// and asserts an error occurred.
func (h *HarnessRPC) XImportMissionControlAssertErr(
	req *routerrpc.XImportMissionControlRequest) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Router.XImportMissionControl(ctxt, req)
	require.Error(h, err, "expect an error from x import mission control")
}

// BuildRoute makes a RPC call to the node's RouterClient and asserts.
func (h *HarnessRPC) BuildRoute(
	req *routerrpc.BuildRouteRequest) *routerrpc.BuildRouteResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Router.BuildRoute(ctxt, req)
	h.NoError(err, "BuildRoute")

	return resp
}

type InterceptorClient routerrpc.Router_HtlcInterceptorClient

// HtlcInterceptor makes a RPC call to the node's RouterClient and asserts.
func (h *HarnessRPC) HtlcInterceptor() (InterceptorClient, context.CancelFunc) {
	// HtlcInterceptor needs to have the context alive for the entire test
	// case as the returned client will be used for send and receive events
	// stream. Thus we use cancel context here instead of a timeout
	// context.
	ctxt, cancel := context.WithCancel(h.runCtx)
	resp, err := h.Router.HtlcInterceptor(ctxt)
	h.NoError(err, "HtlcInterceptor")

	return resp, cancel
}

// XAddLocalChanAliases adds a list of aliases to the node's alias map.
func (h *HarnessRPC) XAddLocalChanAliases(req *routerrpc.AddAliasesRequest) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Router.XAddLocalChanAliases(ctxt, req)
	h.NoError(err, "XAddLocalChanAliases")
}

// XAddLocalChanAliasesErr adds a list of aliases to the node's alias map and
// expects an error.
func (h *HarnessRPC) XAddLocalChanAliasesErr(
	req *routerrpc.AddAliasesRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Router.XAddLocalChanAliases(ctxt, req)
	require.Error(h, err)

	return err
}

// XDeleteLocalChanAliases deleted a set of alias mappings.
func (h *HarnessRPC) XDeleteLocalChanAliases(
	req *routerrpc.DeleteAliasesRequest) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Router.XDeleteLocalChanAliases(ctxt, req)
	h.NoError(err, "XDeleteLocalChanAliases")
}

// XDeleteLocalChanAliasesErr deleted a set of alias mappings and expects an
// error.
func (h *HarnessRPC) XDeleteLocalChanAliasesErr(
	req *routerrpc.DeleteAliasesRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Router.XDeleteLocalChanAliases(ctxt, req)
	require.Error(h, err)

	return err
}

type TrackPaymentsClient routerrpc.Router_TrackPaymentsClient

// TrackPayments makes a RPC call to the node's RouterClient and asserts.
func (h *HarnessRPC) TrackPayments(
	req *routerrpc.TrackPaymentsRequest) TrackPaymentsClient {

	resp, err := h.Router.TrackPayments(h.runCtx, req)
	h.NoError(err, "TrackPayments")

	return resp
}

type TrackPaymentClient routerrpc.Router_TrackPaymentV2Client

// TrackPaymentV2 creates a subscription client for given invoice and
// asserts its creation.
func (h *HarnessRPC) TrackPaymentV2(payHash []byte) TrackPaymentClient {
	req := &routerrpc.TrackPaymentRequest{PaymentHash: payHash}

	// TrackPaymentV2 needs to have the context alive for the entire test
	// case as the returned client will be used for send and receive events
	// stream. Thus we use runCtx here instead of a timeout context.
	client, err := h.Router.TrackPaymentV2(h.runCtx, req)
	h.NoError(err, "TrackPaymentV2")

	return client
}
