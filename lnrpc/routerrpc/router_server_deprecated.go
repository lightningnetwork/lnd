package routerrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// legacyTrackPaymentServer is a wrapper struct that transforms a stream of main
// rpc payment structs into the legacy PaymentStatus format.
type legacyTrackPaymentServer struct {
	Router_TrackPaymentServer
}

// Send converts a Payment object and sends it as a PaymentStatus object on the
// embedded stream.
func (i *legacyTrackPaymentServer) Send(p *lnrpc.Payment) error {
	var state PaymentState
	switch p.Status {
	case lnrpc.Payment_IN_FLIGHT:
		state = PaymentState_IN_FLIGHT
	case lnrpc.Payment_SUCCEEDED:
		state = PaymentState_SUCCEEDED
	case lnrpc.Payment_FAILED:
		switch p.FailureReason {
		case lnrpc.PaymentFailureReason_FAILURE_REASON_NONE:
			return fmt.Errorf("expected fail reason")

		case lnrpc.PaymentFailureReason_FAILURE_REASON_TIMEOUT:
			state = PaymentState_FAILED_TIMEOUT

		case lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE:
			state = PaymentState_FAILED_NO_ROUTE

		case lnrpc.PaymentFailureReason_FAILURE_REASON_ERROR:
			state = PaymentState_FAILED_ERROR

		case lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS:
			state = PaymentState_FAILED_INCORRECT_PAYMENT_DETAILS

		case lnrpc.PaymentFailureReason_FAILURE_REASON_INSUFFICIENT_BALANCE:
			state = PaymentState_FAILED_INSUFFICIENT_BALANCE

		default:
			return fmt.Errorf("unknown failure reason %v",
				p.FailureReason)
		}
	default:
		return fmt.Errorf("unknown state %v", p.Status)
	}

	preimage, err := hex.DecodeString(p.PaymentPreimage)
	if err != nil {
		return err
	}

	legacyState := PaymentStatus{
		State:    state,
		Preimage: preimage,
		Htlcs:    p.Htlcs,
	}

	return i.Router_TrackPaymentServer.Send(&legacyState)
}

// TrackPayment returns a stream of payment state updates. The stream is
// closed when the payment completes.
func (s *Server) TrackPayment(request *TrackPaymentRequest,
	stream Router_TrackPaymentServer) error {

	legacyStream := legacyTrackPaymentServer{
		Router_TrackPaymentServer: stream,
	}
	return s.TrackPaymentV2(request, &legacyStream)
}

// SendPayment attempts to route a payment described by the passed
// PaymentRequest to the final destination. If we are unable to route the
// payment, or cannot find a route that satisfies the constraints in the
// PaymentRequest, then an error will be returned. Otherwise, the payment
// pre-image, along with the final route will be returned.
func (s *Server) SendPayment(request *SendPaymentRequest,
	stream Router_SendPaymentServer) error {

	if request.MaxParts > 1 {
		return errors.New("for multi-part payments, use SendPaymentV2")
	}

	legacyStream := legacyTrackPaymentServer{
		Router_TrackPaymentServer: stream,
	}
	return s.SendPaymentV2(request, &legacyStream)
}

// SendToRoute sends a payment through a predefined route. The response of this
// call contains structured error information.
func (s *Server) SendToRoute(ctx context.Context,
	req *SendToRouteRequest) (*SendToRouteResponse, error) {

	resp, err := s.SendToRouteV2(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp == nil {
		return nil, nil
	}

	// Need to convert to legacy response message because proto identifiers
	// don't line up.
	legacyResp := &SendToRouteResponse{
		Preimage: resp.Preimage,
		Failure:  resp.Failure,
	}

	return legacyResp, nil
}

// QueryProbability returns the current success probability estimate for a
// given node pair and amount.
func (s *Server) QueryProbability(_ context.Context,
	req *QueryProbabilityRequest) (*QueryProbabilityResponse, error) {

	fromNode, err := route.NewVertexFromBytes(req.FromNode)
	if err != nil {
		return nil, err
	}

	toNode, err := route.NewVertexFromBytes(req.ToNode)
	if err != nil {
		return nil, err
	}

	amt := lnwire.MilliSatoshi(req.AmtMsat)

	// Compute the probability.
	var prob float64
	mc := s.cfg.RouterBackend.MissionControl
	capacity, err := s.cfg.RouterBackend.FetchAmountPairCapacity(
		fromNode, toNode, amt,
	)

	// If we cannot query the capacity this means that either we don't have
	// information available or that the channel fails min/maxHtlc
	// constraints, so we return a zero probability.
	if err != nil {
		log.Errorf("Cannot fetch capacity: %v", err)
	} else {
		prob = mc.GetProbability(fromNode, toNode, amt, capacity)
	}

	history := mc.GetPairHistorySnapshot(fromNode, toNode)

	return &QueryProbabilityResponse{
		Probability: prob,
		History:     toRPCPairData(&history),
	}, nil
}
