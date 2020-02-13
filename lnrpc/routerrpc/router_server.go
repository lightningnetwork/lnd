// +build routerrpc

package routerrpc

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize as the name of our
	subServerName = "RouterRPC"
)

var (
	// macaroonOps are the set of capabilities that our minted macaroon (if
	// it doesn't already exist) will have.
	macaroonOps = []bakery.Op{
		{
			Entity: "offchain",
			Action: "read",
		},
		{
			Entity: "offchain",
			Action: "write",
		},
	}

	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/routerrpc.Router/SendPayment": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/SendToRoute": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/TrackPayment": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/routerrpc.Router/EstimateRouteFee": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/routerrpc.Router/QueryMissionControl": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/routerrpc.Router/QueryProbability": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/routerrpc.Router/ResetMissionControl": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/BuildRoute": {{
			Entity: "offchain",
			Action: "read",
		}},
	}

	// DefaultRouterMacFilename is the default name of the router macaroon
	// that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultRouterMacFilename = "router.macaroon"
)

// Server is a stand alone sub RPC server which exposes functionality that
// allows clients to route arbitrary payment through the Lightning Network.
type Server struct {
	cfg *Config
}

// A compile time check to ensure that Server fully implements the RouterServer
// gRPC service.
var _ RouterServer = (*Server)(nil)

// fileExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// New creates a new instance of the RouterServer given a configuration struct
// that contains all external dependencies. If the target macaroon exists, and
// we're unable to create it, then an error will be returned. We also return
// the set of permissions that we require as a server. At the time of writing
// of this documentation, this is the same macaroon as as the admin macaroon.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// If the path of the router macaroon wasn't generated, then we'll
	// assume that it's found at the default network directory.
	if cfg.RouterMacPath == "" {
		cfg.RouterMacPath = filepath.Join(
			cfg.NetworkDir, DefaultRouterMacFilename,
		)
	}

	// Now that we know the full path of the router macaroon, we can check
	// to see if we need to create it or not.
	macFilePath := cfg.RouterMacPath
	if !fileExists(macFilePath) && cfg.MacService != nil {
		log.Infof("Making macaroons for Router RPC Server at: %v",
			macFilePath)

		// At this point, we know that the router macaroon doesn't yet,
		// exist, so we need to create it with the help of the main
		// macaroon service.
		routerMac, err := cfg.MacService.Oven.NewMacaroon(
			context.Background(), bakery.LatestVersion, nil,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		routerMacBytes, err := routerMac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = ioutil.WriteFile(macFilePath, routerMacBytes, 0644)
		if err != nil {
			os.Remove(macFilePath)
			return nil, nil, err
		}
	}

	routerServer := &Server{
		cfg: cfg,
	}

	return routerServer, macPermissions, nil
}

// Start launches any helper goroutines required for the rpcServer to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until this
// is called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterRouterServer(grpcServer, s)

	log.Debugf("Router RPC server successfully register with root gRPC " +
		"server")

	return nil
}

// SendPayment attempts to route a payment described by the passed
// PaymentRequest to the final destination. If we are unable to route the
// payment, or cannot find a route that satisfies the constraints in the
// PaymentRequest, then an error will be returned. Otherwise, the payment
// pre-image, along with the final route will be returned.
func (s *Server) SendPayment(req *SendPaymentRequest,
	stream Router_SendPaymentServer) error {

	payment, err := s.cfg.RouterBackend.extractIntentFromSendRequest(req)
	if err != nil {
		return err
	}

	err = s.cfg.Router.SendPaymentAsync(payment)
	if err != nil {
		// Transform user errors to grpc code.
		if err == channeldb.ErrPaymentInFlight ||
			err == channeldb.ErrAlreadyPaid {

			log.Debugf("SendPayment async result for hash %x: %v",
				payment.PaymentHash, err)

			return status.Error(
				codes.AlreadyExists, err.Error(),
			)
		}

		log.Errorf("SendPayment async error for hash %x: %v",
			payment.PaymentHash, err)

		return err
	}

	return s.trackPayment(payment.PaymentHash, stream)
}

// EstimateRouteFee allows callers to obtain a lower bound w.r.t how much it
// may cost to send an HTLC to the target end destination.
func (s *Server) EstimateRouteFee(ctx context.Context,
	req *RouteFeeRequest) (*RouteFeeResponse, error) {

	if len(req.Dest) != 33 {
		return nil, errors.New("invalid length destination key")
	}
	var destNode route.Vertex
	copy(destNode[:], req.Dest)

	// Next, we'll convert the amount in satoshis to mSAT, which are the
	// native unit of LN.
	amtMsat := lnwire.NewMSatFromSatoshis(btcutil.Amount(req.AmtSat))

	// Pick a fee limit
	//
	// TODO: Change this into behaviour that makes more sense.
	feeLimit := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)

	// Finally, we'll query for a route to the destination that can carry
	// that target amount, we'll only request a single route. Set a
	// restriction for the default CLTV limit, otherwise we can find a route
	// that exceeds it and is useless to us.
	route, err := s.cfg.Router.FindRoute(
		s.cfg.RouterBackend.SelfNode, destNode, amtMsat,
		&routing.RestrictParams{
			FeeLimit:  feeLimit,
			CltvLimit: s.cfg.RouterBackend.MaxTotalTimelock,
		}, nil, nil, s.cfg.RouterBackend.DefaultFinalCltvDelta,
	)
	if err != nil {
		return nil, err
	}

	return &RouteFeeResponse{
		RoutingFeeMsat: int64(route.TotalFees()),
		TimeLockDelay:  int64(route.TotalTimeLock),
	}, nil
}

// SendToRoute sends a payment through a predefined route. The response of this
// call contains structured error information.
func (s *Server) SendToRoute(ctx context.Context,
	req *SendToRouteRequest) (*SendToRouteResponse, error) {

	if req.Route == nil {
		return nil, fmt.Errorf("unable to send, no routes provided")
	}

	route, err := s.cfg.RouterBackend.UnmarshallRoute(req.Route)
	if err != nil {
		return nil, err
	}

	hash, err := lntypes.MakeHash(req.PaymentHash)
	if err != nil {
		return nil, err
	}

	preimage, err := s.cfg.Router.SendToRoute(hash, route)

	// In the success case, return the preimage.
	if err == nil {
		return &SendToRouteResponse{
			Preimage: preimage[:],
		}, nil
	}

	// In the failure case, marshall the failure message to the rpc format
	// before returning it to the caller.
	rpcErr, err := marshallError(err)
	if err != nil {
		return nil, err
	}

	return &SendToRouteResponse{
		Failure: rpcErr,
	}, nil
}

// marshallError marshall an error as received from the switch to rpc structs
// suitable for returning to the caller of an rpc method.
//
// Because of difficulties with using protobuf oneof constructs in some
// languages, the decision was made here to use a single message format for all
// failure messages with some fields left empty depending on the failure type.
func marshallError(sendError error) (*lnrpc.Failure, error) {
	response := &lnrpc.Failure{}

	if sendError == htlcswitch.ErrUnreadableFailureMessage {
		response.Code = lnrpc.Failure_UNREADABLE_FAILURE
		return response, nil
	}

	rtErr, ok := sendError.(htlcswitch.ClearTextError)
	if !ok {
		return nil, sendError
	}

	switch onionErr := rtErr.WireMessage().(type) {

	case *lnwire.FailIncorrectDetails:
		response.Code = lnrpc.Failure_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS
		response.Height = onionErr.Height()

	case *lnwire.FailIncorrectPaymentAmount:
		response.Code = lnrpc.Failure_INCORRECT_PAYMENT_AMOUNT

	case *lnwire.FailFinalIncorrectCltvExpiry:
		response.Code = lnrpc.Failure_FINAL_INCORRECT_CLTV_EXPIRY
		response.CltvExpiry = onionErr.CltvExpiry

	case *lnwire.FailFinalIncorrectHtlcAmount:
		response.Code = lnrpc.Failure_FINAL_INCORRECT_HTLC_AMOUNT
		response.HtlcMsat = uint64(onionErr.IncomingHTLCAmount)

	case *lnwire.FailFinalExpiryTooSoon:
		response.Code = lnrpc.Failure_FINAL_EXPIRY_TOO_SOON

	case *lnwire.FailInvalidRealm:
		response.Code = lnrpc.Failure_INVALID_REALM

	case *lnwire.FailExpiryTooSoon:
		response.Code = lnrpc.Failure_EXPIRY_TOO_SOON
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)

	case *lnwire.FailExpiryTooFar:
		response.Code = lnrpc.Failure_EXPIRY_TOO_FAR

	case *lnwire.FailInvalidOnionVersion:
		response.Code = lnrpc.Failure_INVALID_ONION_VERSION
		response.OnionSha_256 = onionErr.OnionSHA256[:]

	case *lnwire.FailInvalidOnionHmac:
		response.Code = lnrpc.Failure_INVALID_ONION_HMAC
		response.OnionSha_256 = onionErr.OnionSHA256[:]

	case *lnwire.FailInvalidOnionKey:
		response.Code = lnrpc.Failure_INVALID_ONION_KEY
		response.OnionSha_256 = onionErr.OnionSHA256[:]

	case *lnwire.FailAmountBelowMinimum:
		response.Code = lnrpc.Failure_AMOUNT_BELOW_MINIMUM
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)
		response.HtlcMsat = uint64(onionErr.HtlcMsat)

	case *lnwire.FailFeeInsufficient:
		response.Code = lnrpc.Failure_FEE_INSUFFICIENT
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)
		response.HtlcMsat = uint64(onionErr.HtlcMsat)

	case *lnwire.FailIncorrectCltvExpiry:
		response.Code = lnrpc.Failure_INCORRECT_CLTV_EXPIRY
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)
		response.CltvExpiry = onionErr.CltvExpiry

	case *lnwire.FailChannelDisabled:
		response.Code = lnrpc.Failure_CHANNEL_DISABLED
		response.ChannelUpdate = marshallChannelUpdate(&onionErr.Update)
		response.Flags = uint32(onionErr.Flags)

	case *lnwire.FailTemporaryChannelFailure:
		response.Code = lnrpc.Failure_TEMPORARY_CHANNEL_FAILURE
		response.ChannelUpdate = marshallChannelUpdate(onionErr.Update)

	case *lnwire.FailRequiredNodeFeatureMissing:
		response.Code = lnrpc.Failure_REQUIRED_NODE_FEATURE_MISSING

	case *lnwire.FailRequiredChannelFeatureMissing:
		response.Code = lnrpc.Failure_REQUIRED_CHANNEL_FEATURE_MISSING

	case *lnwire.FailUnknownNextPeer:
		response.Code = lnrpc.Failure_UNKNOWN_NEXT_PEER

	case *lnwire.FailTemporaryNodeFailure:
		response.Code = lnrpc.Failure_TEMPORARY_NODE_FAILURE

	case *lnwire.FailPermanentNodeFailure:
		response.Code = lnrpc.Failure_PERMANENT_NODE_FAILURE

	case *lnwire.FailPermanentChannelFailure:
		response.Code = lnrpc.Failure_PERMANENT_CHANNEL_FAILURE

	case *lnwire.FailMPPTimeout:
		response.Code = lnrpc.Failure_MPP_TIMEOUT

	case nil:
		response.Code = lnrpc.Failure_UNKNOWN_FAILURE

	default:
		return nil, fmt.Errorf("cannot marshall failure %T", onionErr)
	}

	// If the ClearTextError received is a ForwardingError, the error
	// originated from a node along the route, not locally on our outgoing
	// link. We set failureSourceIdx to the index of the node where the
	// failure occurred. If the error is not a ForwardingError, the failure
	// occurred at our node, so we leave the index as 0 to indicate that
	// we failed locally.
	fErr, ok := rtErr.(*htlcswitch.ForwardingError)
	if ok {
		response.FailureSourceIndex = uint32(fErr.FailureSourceIdx)
	}

	return response, nil
}

// marshallChannelUpdate marshalls a channel update as received over the wire to
// the router rpc format.
func marshallChannelUpdate(update *lnwire.ChannelUpdate) *lnrpc.ChannelUpdate {
	if update == nil {
		return nil
	}

	return &lnrpc.ChannelUpdate{
		Signature:       update.Signature[:],
		ChainHash:       update.ChainHash[:],
		ChanId:          update.ShortChannelID.ToUint64(),
		Timestamp:       update.Timestamp,
		MessageFlags:    uint32(update.MessageFlags),
		ChannelFlags:    uint32(update.ChannelFlags),
		TimeLockDelta:   uint32(update.TimeLockDelta),
		HtlcMinimumMsat: uint64(update.HtlcMinimumMsat),
		BaseFee:         update.BaseFee,
		FeeRate:         update.FeeRate,
		HtlcMaximumMsat: uint64(update.HtlcMaximumMsat),
		ExtraOpaqueData: update.ExtraOpaqueData,
	}
}

// ResetMissionControl clears all mission control state and starts with a clean
// slate.
func (s *Server) ResetMissionControl(ctx context.Context,
	req *ResetMissionControlRequest) (*ResetMissionControlResponse, error) {

	err := s.cfg.RouterBackend.MissionControl.ResetHistory()
	if err != nil {
		return nil, err
	}

	return &ResetMissionControlResponse{}, nil
}

// QueryMissionControl exposes the internal mission control state to callers. It
// is a development feature.
func (s *Server) QueryMissionControl(ctx context.Context,
	req *QueryMissionControlRequest) (*QueryMissionControlResponse, error) {

	snapshot := s.cfg.RouterBackend.MissionControl.GetHistorySnapshot()

	rpcPairs := make([]*PairHistory, 0, len(snapshot.Pairs))
	for _, p := range snapshot.Pairs {
		// Prevent binding to loop variable.
		pair := p

		rpcPair := PairHistory{
			NodeFrom: pair.Pair.From[:],
			NodeTo:   pair.Pair.To[:],
			History:  toRPCPairData(&pair.TimedPairResult),
		}

		rpcPairs = append(rpcPairs, &rpcPair)
	}

	response := QueryMissionControlResponse{
		Pairs: rpcPairs,
	}

	return &response, nil
}

// toRPCPairData marshalls mission control pair data to the rpc struct.
func toRPCPairData(data *routing.TimedPairResult) *PairData {
	rpcData := PairData{
		FailAmtSat:     int64(data.FailAmt.ToSatoshis()),
		FailAmtMsat:    int64(data.FailAmt),
		SuccessAmtSat:  int64(data.SuccessAmt.ToSatoshis()),
		SuccessAmtMsat: int64(data.SuccessAmt),
	}

	if !data.FailTime.IsZero() {
		rpcData.FailTime = data.FailTime.Unix()
	}

	if !data.SuccessTime.IsZero() {
		rpcData.SuccessTime = data.SuccessTime.Unix()
	}

	return &rpcData
}

// QueryProbability returns the current success probability estimate for a
// given node pair and amount.
func (s *Server) QueryProbability(ctx context.Context,
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

	mc := s.cfg.RouterBackend.MissionControl
	prob := mc.GetProbability(fromNode, toNode, amt)
	history := mc.GetPairHistorySnapshot(fromNode, toNode)

	return &QueryProbabilityResponse{
		Probability: prob,
		History:     toRPCPairData(&history),
	}, nil
}

// TrackPayment returns a stream of payment state updates. The stream is
// closed when the payment completes.
func (s *Server) TrackPayment(request *TrackPaymentRequest,
	stream Router_TrackPaymentServer) error {

	paymentHash, err := lntypes.MakeHash(request.PaymentHash)
	if err != nil {
		return err
	}

	log.Debugf("TrackPayment called for payment %v", paymentHash)

	return s.trackPayment(paymentHash, stream)
}

// trackPayment writes payment status updates to the provided stream.
func (s *Server) trackPayment(paymentHash lntypes.Hash,
	stream Router_TrackPaymentServer) error {

	router := s.cfg.RouterBackend

	// Subscribe to the outcome of this payment.
	inFlight, resultChan, err := router.Tower.SubscribePayment(
		paymentHash,
	)
	switch {
	case err == channeldb.ErrPaymentNotInitiated:
		return status.Error(codes.NotFound, err.Error())
	case err != nil:
		return err
	}

	// If it is in flight, send a state update to the client. Payment status
	// update streams are expected to always send the current payment state
	// immediately.
	if inFlight {
		err = stream.Send(&PaymentStatus{
			State: PaymentState_IN_FLIGHT,
		})
		if err != nil {
			return err
		}
	}

	// Wait for the outcome of the payment. For payments that have
	// completed, the result should already be waiting on the channel.
	select {
	case result := <-resultChan:
		// Marshall result to rpc type.
		var status PaymentStatus
		if result.Success {
			log.Debugf("Payment %v successfully completed",
				paymentHash)

			status.State = PaymentState_SUCCEEDED
			status.Preimage = result.Preimage[:]
		} else {
			state, err := marshallFailureReason(
				result.FailureReason,
			)
			if err != nil {
				return err
			}
			status.State = state
		}

		// Extract the last route from the given list of HTLCs. This
		// will populate the legacy route field for backwards
		// compatibility.
		//
		// NOTE: For now there will be at most one HTLC, this code
		// should be revisted or the field removed when multiple HTLCs
		// are permitted.
		var legacyRoute *route.Route
		for _, htlc := range result.HTLCs {
			switch {
			case htlc.Settle != nil:
				legacyRoute = &htlc.Route

			// Only display the route for failed payments if we got
			// an incorrect payment details error, so that it can be
			// used for probing or fee estimation.
			case htlc.Failure != nil && result.FailureReason ==
				channeldb.FailureReasonPaymentDetails:

				legacyRoute = &htlc.Route
			}
		}
		if legacyRoute != nil {
			status.Route, err = router.MarshallRoute(legacyRoute)
			if err != nil {
				return err
			}
		}

		// Marshal our list of HTLCs that have been tried for this
		// payment.
		htlcs := make([]*lnrpc.HTLCAttempt, 0, len(result.HTLCs))
		for _, dbHtlc := range result.HTLCs {
			htlc, err := router.MarshalHTLCAttempt(dbHtlc)
			if err != nil {
				return err
			}

			htlcs = append(htlcs, htlc)
		}
		status.Htlcs = htlcs

		// Send event to the client.
		err = stream.Send(&status)
		if err != nil {
			return err
		}

	case <-stream.Context().Done():
		log.Debugf("Payment status stream %v canceled", paymentHash)
		return stream.Context().Err()
	}

	return nil
}

// marshallFailureReason marshalls the failure reason to the corresponding rpc
// type.
func marshallFailureReason(reason channeldb.FailureReason) (
	PaymentState, error) {

	switch reason {

	case channeldb.FailureReasonTimeout:
		return PaymentState_FAILED_TIMEOUT, nil

	case channeldb.FailureReasonNoRoute:
		return PaymentState_FAILED_NO_ROUTE, nil

	case channeldb.FailureReasonError:
		return PaymentState_FAILED_ERROR, nil

	case channeldb.FailureReasonPaymentDetails:
		return PaymentState_FAILED_INCORRECT_PAYMENT_DETAILS, nil

	case channeldb.FailureReasonInsufficientBalance:
		return PaymentState_FAILED_INSUFFICIENT_BALANCE, nil
	}

	return 0, errors.New("unknown failure reason")
}

// BuildRoute builds a route from a list of hop addresses.
func (s *Server) BuildRoute(ctx context.Context,
	req *BuildRouteRequest) (*BuildRouteResponse, error) {

	// Unmarshall hop list.
	hops := make([]route.Vertex, len(req.HopPubkeys))
	for i, pubkeyBytes := range req.HopPubkeys {
		pubkey, err := route.NewVertexFromBytes(pubkeyBytes)
		if err != nil {
			return nil, err
		}
		hops[i] = pubkey
	}

	// Prepare BuildRoute call parameters from rpc request.
	var amt *lnwire.MilliSatoshi
	if req.AmtMsat != 0 {
		rpcAmt := lnwire.MilliSatoshi(req.AmtMsat)
		amt = &rpcAmt
	}

	var outgoingChan *uint64
	if req.OutgoingChanId != 0 {
		outgoingChan = &req.OutgoingChanId
	}

	// Build the route and return it to the caller.
	route, err := s.cfg.Router.BuildRoute(
		amt, hops, outgoingChan, req.FinalCltvDelta,
	)
	if err != nil {
		return nil, err
	}

	rpcRoute, err := s.cfg.RouterBackend.MarshallRoute(route)
	if err != nil {
		return nil, err
	}

	routeResp := &BuildRouteResponse{
		Route: rpcRoute,
	}

	return routeResp, nil
}
