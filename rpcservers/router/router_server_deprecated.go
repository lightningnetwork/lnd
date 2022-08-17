package router

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
)

// legacyTrackPaymentServer is a wrapper struct that transforms a stream of main
// rpc payment structs into the legacy PaymentStatus format.
type legacyTrackPaymentServer struct {
	routerrpc.Router_TrackPaymentServer
}

// Send converts a Payment object and sends it as a PaymentStatus object on the
// embedded stream.
func (i *legacyTrackPaymentServer) Send(p *lnrpc.Payment) error {
	var state routerrpc.PaymentState
	switch p.Status {
	case lnrpc.Payment_IN_FLIGHT:
		state = routerrpc.PaymentState_IN_FLIGHT
	case lnrpc.Payment_SUCCEEDED:
		state = routerrpc.PaymentState_SUCCEEDED
	case lnrpc.Payment_FAILED:
		switch p.FailureReason {
		case lnrpc.PaymentFailureReason_FAILURE_REASON_NONE:
			return fmt.Errorf("expected fail reason")

		case lnrpc.PaymentFailureReason_FAILURE_REASON_TIMEOUT:
			state = routerrpc.PaymentState_FAILED_TIMEOUT

		case lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE:
			state = routerrpc.PaymentState_FAILED_NO_ROUTE

		case lnrpc.PaymentFailureReason_FAILURE_REASON_ERROR:
			state = routerrpc.PaymentState_FAILED_ERROR

		case lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS:
			state = routerrpc.PaymentState_FAILED_INCORRECT_PAYMENT_DETAILS

		case lnrpc.PaymentFailureReason_FAILURE_REASON_INSUFFICIENT_BALANCE:
			state = routerrpc.PaymentState_FAILED_INSUFFICIENT_BALANCE

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

	legacyState := routerrpc.PaymentStatus{
		State:    state,
		Preimage: preimage,
		Htlcs:    p.Htlcs,
	}

	return i.Router_TrackPaymentServer.Send(&legacyState)
}

// TrackPayment returns a stream of payment state updates. The stream is
// closed when the payment completes.
func (s *Server) TrackPayment(request *routerrpc.TrackPaymentRequest,
	stream routerrpc.Router_TrackPaymentServer) error {

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
func (s *Server) SendPayment(request *routerrpc.SendPaymentRequest,
	stream routerrpc.Router_SendPaymentServer) error {

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
	req *routerrpc.SendToRouteRequest) (*routerrpc.SendToRouteResponse,
	error) {

	resp, err := s.SendToRouteV2(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp == nil {
		return nil, nil
	}

	// Need to convert to legacy response message because proto identifiers
	// don't line up.
	legacyResp := &routerrpc.SendToRouteResponse{
		Preimage: resp.Preimage,
		Failure:  resp.Failure,
	}

	return legacyResp, err
}
