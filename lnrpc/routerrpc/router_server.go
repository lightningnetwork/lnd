package routerrpc

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
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

	// routeFeeLimitSat is the maximum routing fee that we allow to occur
	// when estimating a routing fee.
	routeFeeLimitSat = 100_000_000

	// DefaultPaymentTimeout is the default value of time we should spend
	// when attempting to fulfill the payment.
	DefaultPaymentTimeout int32 = 60
)

var (
	errServerShuttingDown = errors.New("routerrpc server shutting down")

	// ErrInterceptorAlreadyExists is an error returned when a new stream is
	// opened and there is already one active interceptor. The user must
	// disconnect prior to open another stream.
	ErrInterceptorAlreadyExists = errors.New("interceptor already exists")

	errMissingPaymentAttempt = errors.New("missing payment attempt")

	errMissingRoute = errors.New("missing route")

	errUnexpectedFailureSource = errors.New("unexpected failure source")

	// ErrAliasAlreadyExists is returned if a new SCID alias is attempted
	// to be added that already exists.
	ErrAliasAlreadyExists = errors.New("alias already exists")

	// ErrNoValidAlias is returned if an alias is not in the valid range for
	// allowed SCID aliases.
	ErrNoValidAlias = errors.New("not a valid alias")

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
		"/routerrpc.Router/SendPaymentV2": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/SendToRouteV2": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/SendToRoute": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/TrackPaymentV2": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/routerrpc.Router/TrackPayments": {{
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
		"/routerrpc.Router/XImportMissionControl": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/GetMissionControlConfig": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/routerrpc.Router/SetMissionControlConfig": {{
			Entity: "offchain",
			Action: "write",
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
		"/routerrpc.Router/SubscribeHtlcEvents": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/routerrpc.Router/SendPayment": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/TrackPayment": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/routerrpc.Router/HtlcInterceptor": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/UpdateChanStatus": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/XAddLocalChanAliases": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerrpc.Router/XDeleteLocalChanAliases": {{
			Entity: "offchain",
			Action: "write",
		}},
	}

	// DefaultRouterMacFilename is the default name of the router macaroon
	// that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultRouterMacFilename = "router.macaroon"
)

// FetchChannelEndpoints returns the pubkeys of both endpoints of the
// given channel id if it exists in the graph.
type FetchChannelEndpoints func(chanID uint64) (route.Vertex, route.Vertex,
	error)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	RouterServer
}

// Server is a stand-alone sub RPC server which exposes functionality that
// allows clients to route arbitrary payment through the Lightning Network.
type Server struct {
	started                  int32 // To be used atomically.
	shutdown                 int32 // To be used atomically.
	forwardInterceptorActive int32 // To be used atomically.

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	UnimplementedRouterServer

	cfg *Config

	quit chan struct{}
}

// A compile time check to ensure that Server fully implements the RouterServer
// gRPC service.
var _ RouterServer = (*Server)(nil)

// New creates a new instance of the RouterServer given a configuration struct
// that contains all external dependencies. If the target macaroon exists, and
// we're unable to create it, then an error will be returned. We also return
// the set of permissions that we require as a server. At the time of writing
// of this documentation, this is the same macaroon as the admin macaroon.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// If the path of the router macaroon wasn't generated, then we'll
	// assume that it's found at the default network directory.
	if cfg.RouterMacPath == "" {
		cfg.RouterMacPath = filepath.Join(
			cfg.NetworkDir, DefaultRouterMacFilename,
		)
	}

	// Now that we know the full path of the router macaroon, we can check
	// to see if we need to create it or not. If stateless_init is set
	// then we don't write the macaroons.
	macFilePath := cfg.RouterMacPath
	if cfg.MacService != nil && !cfg.MacService.StatelessInit &&
		!lnrpc.FileExists(macFilePath) {

		log.Infof("Making macaroons for Router RPC Server at: %v",
			macFilePath)

		// At this point, we know that the router macaroon doesn't yet,
		// exist, so we need to create it with the help of the main
		// macaroon service.
		routerMac, err := cfg.MacService.NewMacaroon(
			context.Background(), macaroons.DefaultRootKeyID,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		routerMacBytes, err := routerMac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = os.WriteFile(macFilePath, routerMacBytes, 0644)
		if err != nil {
			_ = os.Remove(macFilePath)
			return nil, nil, err
		}
	}

	routerServer := &Server{
		cfg:  cfg,
		quit: make(chan struct{}),
	}

	return routerServer, macPermissions, nil
}

// Start launches any helper goroutines required for the rpcServer to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	if atomic.AddInt32(&s.started, 1) != 1 {
		return nil
	}

	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	close(s.quit)
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
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterRouterServer(grpcServer, r)

	log.Debugf("Router RPC server successfully registered with root gRPC " +
		"server")

	return nil
}

// RegisterWithRestServer will be called by the root REST mux to direct a sub
// RPC server to register itself with the main REST mux server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRestServer(ctx context.Context,
	mux *runtime.ServeMux, dest string, opts []grpc.DialOption) error {

	// We make sure that we register it with the main REST server to ensure
	// all our methods are routed properly.
	err := RegisterRouterHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Router REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("Router REST server successfully registered with " +
		"root REST server")
	return nil
}

// CreateSubServer populates the subserver's dependencies using the passed
// SubServerConfigDispatcher. This method should fully initialize the
// sub-server instance, making it ready for action. It returns the macaroon
// permissions that the sub-server wishes to pass on to the root server for all
// methods routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) CreateSubServer(configRegistry lnrpc.SubServerConfigDispatcher) (
	lnrpc.SubServer, lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.RouterServer = subServer
	return subServer, macPermissions, nil
}

// SendPaymentV2 attempts to route a payment described by the passed
// PaymentRequest to the final destination. If we are unable to route the
// payment, or cannot find a route that satisfies the constraints in the
// PaymentRequest, then an error will be returned. Otherwise, the payment
// pre-image, along with the final route will be returned.
func (s *Server) SendPaymentV2(req *SendPaymentRequest,
	stream Router_SendPaymentV2Server) error {

	// Set payment request attempt timeout.
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = DefaultPaymentTimeout
	}

	payment, err := s.cfg.RouterBackend.extractIntentFromSendRequest(req)
	if err != nil {
		return err
	}

	// Get the payment hash.
	payHash := payment.Identifier()

	// Init the payment in db.
	paySession, shardTracker, err := s.cfg.Router.PreparePayment(payment)
	if err != nil {
		log.Errorf("SendPayment async error for payment %x: %v",
			payment.Identifier(), err)

		// Transform user errors to grpc code.
		if errors.Is(err, paymentsdb.ErrPaymentExists) ||
			errors.Is(err, paymentsdb.ErrPaymentInFlight) ||
			errors.Is(err, paymentsdb.ErrAlreadyPaid) {

			return status.Error(
				codes.AlreadyExists, err.Error(),
			)
		}

		return err
	}

	// Subscribe to the payment before sending it to make sure we won't
	// miss events.
	sub, err := s.subscribePayment(payHash)
	if err != nil {
		return err
	}

	// The payment context is influenced by two user-provided parameters,
	// the cancelable flag and the payment attempt timeout.
	// If the payment is cancelable, we will use the stream context as the
	// payment context. That way, if the user ends the stream, the payment
	// loop will be canceled.
	// The second context parameter is the timeout. If the user provides a
	// timeout, we will additionally wrap the context in a deadline. If the
	// user provided 'cancelable' and ends the stream before the timeout is
	// reached the payment will be canceled.
	ctx := context.Background()
	if req.Cancelable {
		ctx = stream.Context()
	}

	// Send the payment asynchronously.
	s.cfg.Router.SendPaymentAsync(ctx, payment, paySession, shardTracker)

	// Track the payment and return.
	return s.trackPayment(sub, payHash, stream, req.NoInflightUpdates)
}

// EstimateRouteFee allows callers to obtain an expected value w.r.t how much it
// may cost to send an HTLC to the target end destination. This method sends
// probe payments to the target node, based on target invoice parameters and a
// random payment hash that makes it impossible for the target to settle the
// htlc. The probing stops if a user-provided timeout is reached. If provided
// with a destination key and amount, this method will perform a local graph
// based fee estimation.
func (s *Server) EstimateRouteFee(ctx context.Context,
	req *RouteFeeRequest) (*RouteFeeResponse, error) {

	isProbeDestination := len(req.Dest) > 0
	isProbeInvoice := len(req.PaymentRequest) > 0

	switch {
	case isProbeDestination == isProbeInvoice:
		return nil, errors.New("specify either a destination or an " +
			"invoice")

	case isProbeDestination:
		switch {
		case len(req.Dest) != 33:
			return nil, errors.New("invalid length destination key")

		case req.AmtSat <= 0:
			return nil, errors.New("amount must be greater than 0")

		default:
			return s.probeDestination(req.Dest, req.AmtSat)
		}

	case isProbeInvoice:
		return s.probePaymentRequest(
			ctx, req.PaymentRequest, req.Timeout,
		)
	}

	return &RouteFeeResponse{}, nil
}

// probeDestination estimates fees along a route to a destination based on the
// contents of the local graph.
func (s *Server) probeDestination(dest []byte, amtSat int64) (*RouteFeeResponse,
	error) {

	destNode, err := route.NewVertexFromBytes(dest)
	if err != nil {
		return nil, err
	}

	// Next, we'll convert the amount in satoshis to mSAT, which are the
	// native unit of LN.
	amtMsat := lnwire.NewMSatFromSatoshis(btcutil.Amount(amtSat))

	// Finally, we'll query for a route to the destination that can carry
	// that target amount, we'll only request a single route. Set a
	// restriction for the default CLTV limit, otherwise we can find a route
	// that exceeds it and is useless to us.
	mc := s.cfg.RouterBackend.MissionControl
	routeReq, err := routing.NewRouteRequest(
		s.cfg.RouterBackend.SelfNode, &destNode, amtMsat, 0,
		&routing.RestrictParams{
			FeeLimit:          routeFeeLimitSat,
			CltvLimit:         s.cfg.RouterBackend.MaxTotalTimelock,
			ProbabilitySource: mc.GetProbability,
		}, nil, nil, nil, s.cfg.RouterBackend.DefaultFinalCltvDelta,
	)
	if err != nil {
		return nil, err
	}

	route, _, err := s.cfg.Router.FindRoute(routeReq)
	if err != nil {
		return nil, err
	}

	// We are adding a block padding to the total time lock to account for
	// the safety buffer that the payment session will add to the last hop's
	// cltv delta. This is to prevent the htlc from failing if blocks are
	// mined while it is in flight.
	timeLockDelay := route.TotalTimeLock + uint32(routing.BlockPadding)

	return &RouteFeeResponse{
		RoutingFeeMsat: int64(route.TotalFees()),
		TimeLockDelay:  int64(timeLockDelay),
		FailureReason:  lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
	}, nil
}

// probePaymentRequest estimates fees along a route to a destination that is
// specified in an invoice. The estimation duration is limited by a timeout. In
// case that route hints are provided, this method applies a heuristic to
// identify LSPs which might block probe payments. In that case, fees are
// manually calculated and added to the probed fee estimation up until the LSP
// node. If the route hints don't indicate an LSP, they are passed as arguments
// to the SendPayment_V2 method, which enable it to send probe payments to the
// payment request destination.
//
// NOTE: Be aware that because of the special heuristic that is applied to
// identify LSPs, the probe payment might use a different node id as the
// final destination (the assumed LSP node id).
func (s *Server) probePaymentRequest(ctx context.Context, paymentRequest string,
	timeout uint32) (*RouteFeeResponse, error) {

	payReq, err := zpay32.Decode(
		paymentRequest, s.cfg.RouterBackend.ActiveNetParams,
	)
	if err != nil {
		return nil, err
	}

	if payReq.MilliSat == nil || *payReq.MilliSat <= 0 {
		return nil, errors.New("payment request amount must be " +
			"greater than 0")
	}

	// Generate random payment hash, so we can be sure that the target of
	// the probe payment doesn't have the preimage to settle the htlc.
	var paymentHash lntypes.Hash
	_, err = crand.Read(paymentHash[:])
	if err != nil {
		return nil, fmt.Errorf("cannot generate random probe "+
			"preimage: %w", err)
	}

	amtMsat := int64(*payReq.MilliSat)
	probeRequest := &SendPaymentRequest{
		TimeoutSeconds:   int32(timeout),
		Dest:             payReq.Destination.SerializeCompressed(),
		MaxParts:         1,
		AllowSelfPayment: false,
		AmtMsat:          amtMsat,
		PaymentHash:      paymentHash[:],
		FeeLimitSat:      routeFeeLimitSat,
		FinalCltvDelta:   int32(payReq.MinFinalCLTVExpiry()),
		DestFeatures:     MarshalFeatures(payReq.Features),
	}

	// If the payment addresses is specified, then we'll also populate that
	// now as well.
	payReq.PaymentAddr.WhenSome(func(addr [32]byte) {
		copy(probeRequest.PaymentAddr, addr[:])
	})

	hints := payReq.RouteHints

	// If the hints don't indicate an LSP then chances are that our probe
	// payment won't be blocked along the route to the destination. We send
	// a probe payment with unmodified route hints.
	if !isLSP(hints, s.cfg.RouterBackend.FetchChannelEndpoints) {
		log.Infof("No LSP detected, probing destination %x",
			probeRequest.Dest)

		probeRequest.RouteHints = invoicesrpc.CreateRPCRouteHints(hints)
		return s.sendProbePayment(ctx, probeRequest)
	}

	// If the heuristic indicates an LSP we modify the route hints to allow
	// probing the LSP.
	lspAdjustedRouteHints, lspHint, err := prepareLspRouteHints(
		hints, *payReq.MilliSat,
	)
	if err != nil {
		return nil, err
	}

	// Set the destination to the LSP node ID.
	lspDest := lspHint.NodeID.SerializeCompressed()
	probeRequest.Dest = lspDest

	log.Infof("LSP detected, probing LSP with destination: %x", lspDest)

	// The adjusted route hints serve the payment probe to find the last
	// public hop to the LSP on the route.
	if len(lspAdjustedRouteHints) > 0 {
		probeRequest.RouteHints = invoicesrpc.CreateRPCRouteHints(
			lspAdjustedRouteHints,
		)
	}

	// The payment probe will be able to calculate the fee up until the LSP
	// node. The fee of the last hop has to be calculated manually. Since
	// the last hop's fee amount has to be sent across the payment path we
	// have to add it to the original payment amount. Only then will the
	// payment probe be able to determine the correct fee to the last hop
	// prior to the private destination. For example, if the user wants to
	// send 1000 sats to a private destination and the last hop's fee is 10
	// sats, then 1010 sats will have to arrive at the last hop. This means
	// that the probe has to be dispatched with 1010 sats to correctly
	// calculate the routing fee.
	//
	// Calculate the hop fee for the last hop manually.
	hopFee := lspHint.HopFee(*payReq.MilliSat)
	if err != nil {
		return nil, err
	}

	// Add the last hop's fee to the requested payment amount that we want
	// to get an estimate for.
	probeRequest.AmtMsat += int64(hopFee)

	// Use the hop hint's cltv delta as the payment request's final cltv
	// delta. The actual final cltv delta of the invoice will be added to
	// the payment probe's cltv delta.
	probeRequest.FinalCltvDelta = int32(lspHint.CLTVExpiryDelta)

	// Dispatch the payment probe with adjusted fee amount.
	resp, err := s.sendProbePayment(ctx, probeRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to send probe payment to "+
			"LSP with destination %x: %w", lspDest, err)
	}

	// If the payment probe failed we only return the failure reason and
	// leave the probe result params unaltered.
	if resp.FailureReason != lnrpc.PaymentFailureReason_FAILURE_REASON_NONE { //nolint:ll
		return resp, nil
	}

	// The probe succeeded, so we can add the last hop's fee to fee the
	// payment probe returned.
	resp.RoutingFeeMsat += int64(hopFee)

	// Add the final cltv delta of the invoice to the payment probe's total
	// cltv delta. This is the cltv delta for the hop behind the LSP.
	resp.TimeLockDelay += int64(payReq.MinFinalCLTVExpiry())

	return resp, nil
}

// isLSP checks if the route hints indicate an LSP. An LSP is indicated with
// true if the destination hop hint in each route hint has the same node id,
// false otherwise. If the destination hop hint of any route hint contains a
// public channel, the function returns false because we can directly send a
// probe to the final destination.
func isLSP(routeHints [][]zpay32.HopHint,
	fetchChannelEndpoints FetchChannelEndpoints) bool {

	if len(routeHints) == 0 || len(routeHints[0]) == 0 {
		return false
	}

	destHopHint := routeHints[0][len(routeHints[0])-1]

	// If the destination hop hint of the first route hint contains a public
	// channel we can send a probe to it directly, hence we don't signal an
	// LSP.
	_, _, err := fetchChannelEndpoints(destHopHint.ChannelID)
	if err == nil {
		return false
	}

	for i := 1; i < len(routeHints); i++ {
		// Skip empty route hints.
		if len(routeHints[i]) == 0 {
			continue
		}

		lastHop := routeHints[i][len(routeHints[i])-1]

		// If the last hop hint of any route hint contains a public
		// channel we can send a probe to it directly, hence we don't
		// signal an LSP.
		_, _, err = fetchChannelEndpoints(lastHop.ChannelID)
		if err == nil {
			return false
		}

		matchesDestNode := bytes.Equal(
			lastHop.NodeID.SerializeCompressed(),
			destHopHint.NodeID.SerializeCompressed(),
		)
		if !matchesDestNode {
			return false
		}
	}

	// We ensured that the destination hop hint doesn't contain a public
	// channel, and that all destination hop hints of all route hints match,
	// so we signal an LSP.
	return true
}

// prepareLspRouteHints assumes that the isLsp heuristic returned true for the
// route hints passed in here. It constructs a modified list of route hints that
// allows the caller to probe the LSP, which itself is returned as a separate
// hop hint.
func prepareLspRouteHints(routeHints [][]zpay32.HopHint,
	amt lnwire.MilliSatoshi) ([][]zpay32.HopHint, *zpay32.HopHint, error) {

	if len(routeHints) == 0 {
		return nil, nil, fmt.Errorf("no route hints provided")
	}

	// Create the LSP hop hint. We are probing for the worst case fee and
	// cltv delta. So we look for the max values amongst all LSP hop hints.
	refHint := routeHints[0][len(routeHints[0])-1]
	refHint.CLTVExpiryDelta = maxLspCltvDelta(routeHints)
	refHint.FeeBaseMSat, refHint.FeeProportionalMillionths = maxLspFee(
		routeHints, amt,
	)

	// We construct a modified list of route hints that allows the caller to
	// probe the LSP.
	adjustedHints := make([][]zpay32.HopHint, 0, len(routeHints))

	// Strip off the LSP hop hint from all route hints.
	for i := 0; i < len(routeHints); i++ {
		hint := routeHints[i]
		if len(hint) > 1 {
			adjustedHints = append(
				adjustedHints, hint[:len(hint)-1],
			)
		}
	}

	return adjustedHints, &refHint, nil
}

// maxLspFee returns base fee and fee rate amongst all LSP route hints that
// results in the overall highest fee for the given amount.
func maxLspFee(routeHints [][]zpay32.HopHint, amt lnwire.MilliSatoshi) (uint32,
	uint32) {

	var maxFeePpm uint32
	var maxBaseFee uint32
	var maxTotalFee lnwire.MilliSatoshi
	for _, rh := range routeHints {
		lastHop := rh[len(rh)-1]
		lastHopFee := lastHop.HopFee(amt)
		if lastHopFee > maxTotalFee {
			maxTotalFee = lastHopFee
			maxBaseFee = lastHop.FeeBaseMSat
			maxFeePpm = lastHop.FeeProportionalMillionths
		}
	}

	return maxBaseFee, maxFeePpm
}

// maxLspCltvDelta returns the maximum cltv delta amongst all LSP route hints.
func maxLspCltvDelta(routeHints [][]zpay32.HopHint) uint16 {
	var maxCltvDelta uint16
	for _, rh := range routeHints {
		rhLastHop := rh[len(rh)-1]
		if rhLastHop.CLTVExpiryDelta > maxCltvDelta {
			maxCltvDelta = rhLastHop.CLTVExpiryDelta
		}
	}

	return maxCltvDelta
}

// probePaymentStream is a custom implementation of the grpc.ServerStream
// interface. It is used to send payment status updates to the caller on the
// stream channel.
type probePaymentStream struct {
	Router_SendPaymentV2Server

	stream chan *lnrpc.Payment
	ctx    context.Context //nolint:containedctx
}

// Send sends a payment status update to a payment stream that the caller can
// evaluate.
func (p *probePaymentStream) Send(response *lnrpc.Payment) error {
	select {
	case p.stream <- response:

	case <-p.ctx.Done():
		return p.ctx.Err()
	}

	return nil
}

// Context returns the context of the stream.
func (p *probePaymentStream) Context() context.Context {
	return p.ctx
}

// sendProbePayment sends a payment to a target node in order to obtain
// potential routing fees for it. The payment request has to contain a payment
// hash that is guaranteed to be unknown to the target node, so it cannot settle
// the payment. This method invokes a payment request loop in a goroutine and
// awaits payment status updates.
func (s *Server) sendProbePayment(ctx context.Context,
	req *SendPaymentRequest) (*RouteFeeResponse, error) {

	// We'll launch a goroutine to send the payment probes.
	errChan := make(chan error, 1)
	defer close(errChan)

	paymentStream := &probePaymentStream{
		stream: make(chan *lnrpc.Payment),
		ctx:    ctx,
	}
	go func() {
		err := s.SendPaymentV2(req, paymentStream)
		if err != nil {
			select {
			case errChan <- err:

			case <-paymentStream.ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case payment := <-paymentStream.stream:
			switch payment.Status {
			case lnrpc.Payment_INITIATED:
			case lnrpc.Payment_IN_FLIGHT:
			case lnrpc.Payment_SUCCEEDED:
				return nil, errors.New("warning, the fee " +
					"estimation payment probe " +
					"unexpectedly succeeded. Please reach" +
					"out to the probe destination to " +
					"negotiate a refund. Otherwise the " +
					"payment probe amount is lost forever")

			case lnrpc.Payment_FAILED:
				// Incorrect payment details point to a
				// successful probe.
				//nolint:ll
				if payment.FailureReason == lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS {
					return paymentDetails(payment)
				}

				return &RouteFeeResponse{
					RoutingFeeMsat: 0,
					TimeLockDelay:  0,
					FailureReason:  payment.FailureReason,
				}, nil

			default:
				return nil, errors.New("unexpected payment " +
					"status")
			}

		case err := <-errChan:
			return nil, err

		case <-s.quit:
			return nil, errServerShuttingDown
		}
	}
}

func paymentDetails(payment *lnrpc.Payment) (*RouteFeeResponse, error) {
	fee, timeLock, err := timelockAndFee(payment)
	if errors.Is(err, errUnexpectedFailureSource) {
		return nil, err
	}

	return &RouteFeeResponse{
		RoutingFeeMsat: fee,
		TimeLockDelay:  timeLock,
		FailureReason:  lnrpc.PaymentFailureReason_FAILURE_REASON_NONE,
	}, nil
}

// timelockAndFee returns the fee and total time lock of the last payment
// attempt.
func timelockAndFee(p *lnrpc.Payment) (int64, int64, error) {
	if len(p.Htlcs) == 0 {
		return 0, 0, nil
	}

	lastAttempt := p.Htlcs[len(p.Htlcs)-1]
	if lastAttempt == nil {
		return 0, 0, errMissingPaymentAttempt
	}

	lastRoute := lastAttempt.Route
	if lastRoute == nil {
		return 0, 0, errMissingRoute
	}

	hopFailureIndex := lastAttempt.Failure.FailureSourceIndex
	finalHopIndex := uint32(len(lastRoute.Hops))
	if hopFailureIndex != finalHopIndex {
		return 0, 0, errUnexpectedFailureSource
	}

	return lastRoute.TotalFeesMsat, int64(lastRoute.TotalTimeLock), nil
}

// SendToRouteV2 sends a payment through a predefined route. The response of
// this call contains structured error information.
func (s *Server) SendToRouteV2(ctx context.Context,
	req *SendToRouteRequest) (*lnrpc.HTLCAttempt, error) {

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

	firstHopRecords := lnwire.CustomRecords(req.FirstHopCustomRecords)
	if err := firstHopRecords.Validate(); err != nil {
		return nil, err
	}

	var attempt *paymentsdb.HTLCAttempt

	// Pass route to the router. This call returns the full htlc attempt
	// information as it is stored in the database. It is possible that both
	// the attempt return value and err are non-nil. This can happen when
	// the attempt was already initiated before the error happened. In that
	// case, we give precedence to the attempt information as stored in the
	// db.
	if req.SkipTempErr {
		attempt, err = s.cfg.Router.SendToRouteSkipTempErr(
			ctx, hash, route, firstHopRecords,
		)
	} else {
		attempt, err = s.cfg.Router.SendToRoute(
			ctx, hash, route, firstHopRecords,
		)
	}
	if attempt != nil {
		rpcAttempt, err := s.cfg.RouterBackend.MarshalHTLCAttempt(
			*attempt,
		)
		if err != nil {
			return nil, err
		}
		return rpcAttempt, nil
	}

	// Transform user errors to grpc code.
	switch {
	case errors.Is(err, paymentsdb.ErrPaymentExists):
		fallthrough

	case errors.Is(err, paymentsdb.ErrPaymentInFlight):
		fallthrough

	case errors.Is(err, paymentsdb.ErrAlreadyPaid):
		return nil, status.Error(
			codes.AlreadyExists, err.Error(),
		)
	}

	return nil, err
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

// GetMissionControlConfig returns our current mission control config.
func (s *Server) GetMissionControlConfig(ctx context.Context,
	req *GetMissionControlConfigRequest) (*GetMissionControlConfigResponse,
	error) {

	// Query the current mission control config.
	cfg := s.cfg.RouterBackend.MissionControl.GetConfig()
	resp := &GetMissionControlConfigResponse{
		Config: &MissionControlConfig{
			MaximumPaymentResults: uint32(cfg.MaxMcHistory),
			MinimumFailureRelaxInterval: uint64(
				cfg.MinFailureRelaxInterval.Seconds(),
			),
		},
	}

	// We only populate fields based on the current estimator.
	switch v := cfg.Estimator.Config().(type) {
	case routing.AprioriConfig:
		resp.Config.Model = MissionControlConfig_APRIORI
		aCfg := AprioriParameters{
			HalfLifeSeconds:  uint64(v.PenaltyHalfLife.Seconds()),
			HopProbability:   v.AprioriHopProbability,
			Weight:           v.AprioriWeight,
			CapacityFraction: v.CapacityFraction,
		}

		// Populate deprecated fields.
		resp.Config.HalfLifeSeconds = uint64(
			v.PenaltyHalfLife.Seconds(),
		)
		resp.Config.HopProbability = float32(v.AprioriHopProbability)
		resp.Config.Weight = float32(v.AprioriWeight)

		resp.Config.EstimatorConfig = &MissionControlConfig_Apriori{
			Apriori: &aCfg,
		}

	case routing.BimodalConfig:
		resp.Config.Model = MissionControlConfig_BIMODAL
		bCfg := BimodalParameters{
			NodeWeight: v.BimodalNodeWeight,
			ScaleMsat:  uint64(v.BimodalScaleMsat),
			DecayTime:  uint64(v.BimodalDecayTime.Seconds()),
		}

		resp.Config.EstimatorConfig = &MissionControlConfig_Bimodal{
			Bimodal: &bCfg,
		}

	default:
		return nil, fmt.Errorf("unknown estimator config type %T", v)
	}

	return resp, nil
}

// SetMissionControlConfig sets parameters in the mission control config.
func (s *Server) SetMissionControlConfig(ctx context.Context,
	req *SetMissionControlConfigRequest) (*SetMissionControlConfigResponse,
	error) {

	mcCfg := &routing.MissionControlConfig{
		MaxMcHistory: int(req.Config.MaximumPaymentResults),
		MinFailureRelaxInterval: time.Duration(
			req.Config.MinimumFailureRelaxInterval,
		) * time.Second,
	}

	switch req.Config.Model {
	case MissionControlConfig_APRIORI:
		var aprioriConfig routing.AprioriConfig

		// Determine the apriori config with backward compatibility
		// should the api use deprecated fields.
		switch v := req.Config.EstimatorConfig.(type) {
		case *MissionControlConfig_Bimodal:
			return nil, fmt.Errorf("bimodal config " +
				"provided, but apriori model requested")

		case *MissionControlConfig_Apriori:
			aprioriConfig = routing.AprioriConfig{
				PenaltyHalfLife: time.Duration(
					v.Apriori.HalfLifeSeconds,
				) * time.Second,
				AprioriHopProbability: v.Apriori.HopProbability,
				AprioriWeight:         v.Apriori.Weight,
				CapacityFraction: v.Apriori.
					CapacityFraction,
			}

		default:
			aprioriConfig = routing.AprioriConfig{
				PenaltyHalfLife: time.Duration(
					int64(req.Config.HalfLifeSeconds),
				) * time.Second,
				AprioriHopProbability: float64(
					req.Config.HopProbability,
				),
				AprioriWeight:    float64(req.Config.Weight),
				CapacityFraction: routing.DefaultCapacityFraction, //nolint:ll
			}
		}

		estimator, err := routing.NewAprioriEstimator(aprioriConfig)
		if err != nil {
			return nil, err
		}
		mcCfg.Estimator = estimator

	case MissionControlConfig_BIMODAL:
		cfg, ok := req.Config.
			EstimatorConfig.(*MissionControlConfig_Bimodal)
		if !ok {
			return nil, fmt.Errorf("bimodal estimator requested " +
				"but corresponding config not set")
		}
		bCfg := cfg.Bimodal

		bimodalConfig := routing.BimodalConfig{
			BimodalDecayTime: time.Duration(
				bCfg.DecayTime,
			) * time.Second,
			BimodalScaleMsat:  lnwire.MilliSatoshi(bCfg.ScaleMsat),
			BimodalNodeWeight: bCfg.NodeWeight,
		}

		estimator, err := routing.NewBimodalEstimator(bimodalConfig)
		if err != nil {
			return nil, err
		}
		mcCfg.Estimator = estimator

	default:
		return nil, fmt.Errorf("unknown estimator type %v",
			req.Config.Model)
	}

	return &SetMissionControlConfigResponse{},
		s.cfg.RouterBackend.MissionControl.SetConfig(mcCfg)
}

// QueryMissionControl exposes the internal mission control state to callers. It
// is a development feature.
func (s *Server) QueryMissionControl(_ context.Context,
	_ *QueryMissionControlRequest) (*QueryMissionControlResponse, error) {

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

// XImportMissionControl imports the state provided to our internal mission
// control. Only entries that are fresher than our existing state will be used.
func (s *Server) XImportMissionControl(_ context.Context,
	req *XImportMissionControlRequest) (*XImportMissionControlResponse,
	error) {

	if len(req.Pairs) == 0 {
		return nil, errors.New("at least one pair required for import")
	}

	snapshot := &routing.MissionControlSnapshot{
		Pairs: make(
			[]routing.MissionControlPairSnapshot, len(req.Pairs),
		),
	}

	for i, pairResult := range req.Pairs {
		pairSnapshot, err := toPairSnapshot(pairResult)
		if err != nil {
			return nil, err
		}

		snapshot.Pairs[i] = *pairSnapshot
	}

	err := s.cfg.RouterBackend.MissionControl.ImportHistory(
		snapshot, req.Force,
	)
	if err != nil {
		return nil, err
	}

	return &XImportMissionControlResponse{}, nil
}

func toPairSnapshot(pairResult *PairHistory) (*routing.MissionControlPairSnapshot,
	error) {

	from, err := route.NewVertexFromBytes(pairResult.NodeFrom)
	if err != nil {
		return nil, err
	}

	to, err := route.NewVertexFromBytes(pairResult.NodeTo)
	if err != nil {
		return nil, err
	}

	pairPrefix := fmt.Sprintf("pair: %v -> %v:", from, to)

	if from == to {
		return nil, fmt.Errorf("%v source and destination node must "+
			"differ", pairPrefix)
	}

	failAmt, failTime, err := getPair(
		lnwire.MilliSatoshi(pairResult.History.FailAmtMsat),
		btcutil.Amount(pairResult.History.FailAmtSat),
		pairResult.History.FailTime,
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("%v invalid failure: %w", pairPrefix,
			err)
	}

	successAmt, successTime, err := getPair(
		lnwire.MilliSatoshi(pairResult.History.SuccessAmtMsat),
		btcutil.Amount(pairResult.History.SuccessAmtSat),
		pairResult.History.SuccessTime,
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("%v invalid success: %w", pairPrefix,
			err)
	}

	if successAmt == 0 && failAmt == 0 {
		return nil, fmt.Errorf("%v: either success or failure result "+
			"required", pairPrefix)
	}

	pair := routing.NewDirectedNodePair(from, to)

	result := &routing.TimedPairResult{
		FailAmt:     failAmt,
		FailTime:    failTime,
		SuccessAmt:  successAmt,
		SuccessTime: successTime,
	}

	return &routing.MissionControlPairSnapshot{
		Pair:            pair,
		TimedPairResult: *result,
	}, nil
}

// getPair validates the values provided for a mission control result and
// returns the msat amount and timestamp for it. `isFailure` can be used to
// default values to 0 instead of returning an error.
func getPair(amtMsat lnwire.MilliSatoshi, amtSat btcutil.Amount,
	timestamp int64, isFailure bool) (lnwire.MilliSatoshi, time.Time,
	error) {

	amt, err := getMsatPairValue(amtMsat, amtSat)
	if err != nil {
		return 0, time.Time{}, err
	}

	var (
		timeSet   = timestamp != 0
		amountSet = amt != 0
	)

	switch {
	// If a timestamp and amount if provided, return those values.
	case timeSet && amountSet:
		return amt, time.Unix(timestamp, 0), nil

	// Return an error if it does have a timestamp without an amount, and
	// it's not expected to be a failure.
	case !isFailure && timeSet && !amountSet:
		return 0, time.Time{}, errors.New("non-zero timestamp " +
			"requires non-zero amount for success pairs")

	// Return an error if it does have an amount without a timestamp, and
	// it's not expected to be a failure.
	case !isFailure && !timeSet && amountSet:
		return 0, time.Time{}, errors.New("non-zero amount for " +
			"success pairs requires non-zero timestamp")

	default:
		return 0, time.Time{}, nil
	}
}

// getMsatPairValue checks the msat and sat values set for a pair and ensures
// that the values provided are either the same, or only a single value is set.
func getMsatPairValue(msatValue lnwire.MilliSatoshi,
	satValue btcutil.Amount) (lnwire.MilliSatoshi, error) {

	// If our msat value converted to sats equals our sat value, we just
	// return the msat value, since the values are the same.
	if msatValue.ToSatoshis() == satValue {
		return msatValue, nil
	}

	// If we have no msatValue, we can just return our state value even if
	// it is zero, because it's impossible that we have mismatched values.
	if msatValue == 0 {
		return lnwire.MilliSatoshi(satValue * 1000), nil
	}

	// Likewise, we can just use msat value if we have no sat value set.
	if satValue == 0 {
		return msatValue, nil
	}

	// If our values are non-zero but not equal, we have invalid amounts
	// set, so we fail.
	return 0, fmt.Errorf("msat: %v and sat: %v values not equal", msatValue,
		satValue)
}

// TrackPaymentV2 returns a stream of payment state updates. The stream is
// closed when the payment completes.
func (s *Server) TrackPaymentV2(request *TrackPaymentRequest,
	stream Router_TrackPaymentV2Server) error {

	payHash, err := lntypes.MakeHash(request.PaymentHash)
	if err != nil {
		return err
	}

	log.Debugf("TrackPayment called for payment %v", payHash)

	// Make the subscription.
	sub, err := s.subscribePayment(payHash)
	if err != nil {
		return err
	}

	return s.trackPayment(sub, payHash, stream, request.NoInflightUpdates)
}

// subscribePayment subscribes to the payment updates for the given payment
// hash.
func (s *Server) subscribePayment(identifier lntypes.Hash) (
	routing.ControlTowerSubscriber, error) {

	// Make the subscription.
	router := s.cfg.RouterBackend
	sub, err := router.Tower.SubscribePayment(identifier)

	switch {
	case errors.Is(err, paymentsdb.ErrPaymentNotInitiated):
		return nil, status.Error(codes.NotFound, err.Error())

	case err != nil:
		return nil, err
	}

	return sub, nil
}

// trackPayment writes payment status updates to the provided stream.
func (s *Server) trackPayment(subscription routing.ControlTowerSubscriber,
	identifier lntypes.Hash, stream Router_TrackPaymentV2Server,
	noInflightUpdates bool) error {

	err := s.trackPaymentStream(
		stream.Context(), subscription, noInflightUpdates, stream.Send,
	)
	switch {
	case err == nil:
		return nil

	// If the context is canceled, we don't return an error.
	case errors.Is(err, context.Canceled):
		log.Infof("Payment stream %v canceled", identifier)

		return nil

	default:
	}

	// Otherwise, we will log and return the error as the stream has
	// received an error from the payment lifecycle.
	log.Errorf("TrackPayment got error for payment %v: %v", identifier, err)

	return err
}

// TrackPayments returns a stream of payment state updates.
func (s *Server) TrackPayments(request *TrackPaymentsRequest,
	stream Router_TrackPaymentsServer) error {

	log.Debug("TrackPayments called")

	router := s.cfg.RouterBackend

	// Subscribe to payments.
	subscription, err := router.Tower.SubscribeAllPayments()
	if err != nil {
		return err
	}

	// Stream updates to the client.
	err = s.trackPaymentStream(
		stream.Context(), subscription, request.NoInflightUpdates,
		stream.Send,
	)

	if errors.Is(err, context.Canceled) {
		log.Debugf("TrackPayments payment stream canceled.")
	}

	return err
}

// trackPaymentStream streams payment updates to the client.
func (s *Server) trackPaymentStream(context context.Context,
	subscription routing.ControlTowerSubscriber, noInflightUpdates bool,
	send func(*lnrpc.Payment) error) error {

	defer subscription.Close()

	// Stream updates back to the client.
	for {
		select {
		case item, ok := <-subscription.Updates():
			if !ok {
				// No more payment updates.
				return nil
			}
			result, ok := item.(*paymentsdb.MPPayment)
			if !ok {
				return fmt.Errorf("unexpected payment type: %T",
					item)
			}

			log.Tracef("Payment %v updated to state %v",
				result.Info.PaymentIdentifier, result.Status)

			// Skip in-flight updates unless requested.
			if noInflightUpdates {
				if result.Status == paymentsdb.StatusInitiated {
					continue
				}
				if result.Status == paymentsdb.StatusInFlight {
					continue
				}
			}

			rpcPayment, err := s.cfg.RouterBackend.MarshallPayment(
				result,
			)
			if err != nil {
				return err
			}

			// Send event to the client.
			err = send(rpcPayment)
			if err != nil {
				return err
			}

		case <-s.quit:
			return errServerShuttingDown

		case <-context.Done():
			return context.Err()
		}
	}
}

// BuildRoute builds a route from a list of hop addresses.
func (s *Server) BuildRoute(_ context.Context,
	req *BuildRouteRequest) (*BuildRouteResponse, error) {

	if len(req.HopPubkeys) == 0 {
		return nil, errors.New("no hops specified")
	}

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
	var amt fn.Option[lnwire.MilliSatoshi]
	if req.AmtMsat != 0 {
		rpcAmt := lnwire.MilliSatoshi(req.AmtMsat)
		amt = fn.Some(rpcAmt)
	}

	var outgoingChan *uint64
	if req.OutgoingChanId != 0 {
		outgoingChan = &req.OutgoingChanId
	}

	var payAddr fn.Option[[32]byte]
	if len(req.PaymentAddr) != 0 {
		var backingPayAddr [32]byte
		copy(backingPayAddr[:], req.PaymentAddr)

		payAddr = fn.Some(backingPayAddr)
	}

	if req.FinalCltvDelta == 0 {
		req.FinalCltvDelta = int32(
			s.cfg.RouterBackend.DefaultFinalCltvDelta,
		)
	}

	var firstHopBlob fn.Option[[]byte]
	if len(req.FirstHopCustomRecords) > 0 {
		firstHopRecords := lnwire.CustomRecords(
			req.FirstHopCustomRecords,
		)
		if err := firstHopRecords.Validate(); err != nil {
			return nil, err
		}

		firstHopData, err := firstHopRecords.Serialize()
		if err != nil {
			return nil, err
		}
		firstHopBlob = fn.Some(firstHopData)
	}

	// Build the route and return it to the caller.
	route, err := s.cfg.Router.BuildRoute(
		amt, hops, outgoingChan, req.FinalCltvDelta, payAddr,
		firstHopBlob,
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

// SubscribeHtlcEvents creates a uni-directional stream from the server to
// the client which delivers a stream of htlc events.
func (s *Server) SubscribeHtlcEvents(_ *SubscribeHtlcEventsRequest,
	stream Router_SubscribeHtlcEventsServer) error {

	htlcClient, err := s.cfg.RouterBackend.SubscribeHtlcEvents()
	if err != nil {
		return err
	}
	defer htlcClient.Cancel()

	// Send out an initial subscribed event so that the caller knows the
	// point from which new events will be transmitted.
	if err := stream.Send(&HtlcEvent{
		Event: &HtlcEvent_SubscribedEvent{
			SubscribedEvent: &SubscribedEvent{},
		},
	}); err != nil {
		return err
	}

	for {
		select {
		case event := <-htlcClient.Updates():
			rpcEvent, err := rpcHtlcEvent(event)
			if err != nil {
				return err
			}

			if err := stream.Send(rpcEvent); err != nil {
				return err
			}

		// If the stream's context is cancelled, return an error.
		case <-stream.Context().Done():
			log.Debugf("htlc event stream cancelled")
			return stream.Context().Err()

		// If the subscribe client terminates, exit with an error.
		case <-htlcClient.Quit():
			return errors.New("htlc event subscription terminated")

		// If the server has been signalled to shut down, exit.
		case <-s.quit:
			return errServerShuttingDown
		}
	}
}

// HtlcInterceptor is a bidirectional stream for streaming interception
// requests to the caller.
// Upon connection, it does the following:
// 1. Check if there is already a live stream, if yes it rejects the request.
// 2. Registered a ForwardInterceptor
// 3. Delivers to the caller every √√ and detect his answer.
// It uses a local implementation of holdForwardsStore to keep all the hold
// forwards and find them when manual resolution is later needed.
func (s *Server) HtlcInterceptor(stream Router_HtlcInterceptorServer) error {
	// We ensure there is only one interceptor at a time.
	if !atomic.CompareAndSwapInt32(&s.forwardInterceptorActive, 0, 1) {
		return ErrInterceptorAlreadyExists
	}
	defer atomic.CompareAndSwapInt32(&s.forwardInterceptorActive, 1, 0)

	// Run the forward interceptor.
	return newForwardInterceptor(
		s.cfg.RouterBackend.InterceptableForwarder, stream,
	).run()
}

// XAddLocalChanAliases is an experimental API that creates a set of new
// channel SCID alias mappings. The final total set of aliases in the manager
// after the add operation is returned. This is only a locally stored alias, and
// will not be communicated to the channel peer via any message. Therefore,
// routing over such an alias will only work if the peer also calls this same
// RPC on their end. If an alias already exists, an error is returned.
func (s *Server) XAddLocalChanAliases(_ context.Context,
	in *AddAliasesRequest) (*AddAliasesResponse, error) {

	existingAliases := s.cfg.AliasMgr.ListAliases()

	// aliasExists checks if the new alias already exists in the alias map.
	aliasExists := func(newAlias uint64,
		baseScid lnwire.ShortChannelID) (bool, error) {

		// First check that we actually have a channel for the given
		// base scid. This should succeed for any channel where the
		// option-scid-alias feature bit was negotiated.
		if _, ok := existingAliases[baseScid]; !ok {
			return false, fmt.Errorf("base scid %v not found",
				baseScid)
		}

		for base, aliases := range existingAliases {
			for _, alias := range aliases {
				exists := alias.ToUint64() == newAlias

				// Trying to add an alias that we already have
				// for another channel is wrong.
				if exists && base != baseScid {
					return true, fmt.Errorf("%w: alias %v "+
						"already exists for base scid "+
						"%v", ErrAliasAlreadyExists,
						alias, base)
				}

				if exists {
					return true, nil
				}
			}
		}

		return false, nil
	}

	for _, v := range in.AliasMaps {
		baseScid := lnwire.NewShortChanIDFromInt(v.BaseScid)

		for _, rpcAlias := range v.Aliases {
			// If not, let's add it to the alias manager now.
			aliasScid := lnwire.NewShortChanIDFromInt(rpcAlias)

			// But we only add it, if it's a valid alias, as defined
			// by the BOLT spec.
			if !aliasmgr.IsAlias(aliasScid) {
				return nil, fmt.Errorf("%w: SCID alias %v is "+
					"not a valid alias", ErrNoValidAlias,
					aliasScid)
			}

			exists, err := aliasExists(rpcAlias, baseScid)
			if err != nil {
				return nil, err
			}

			// If the alias already exists, we see that as an error.
			// This is to avoid "silent" collisions.
			if exists {
				return nil, fmt.Errorf("%w: SCID alias %v "+
					"already exists", ErrAliasAlreadyExists,
					rpcAlias)
			}

			// We set the baseLookup flag as we want the alias
			// manager to keep a mapping from the alias back to its
			// base scid, in order to be able to provide it via the
			// FindBaseLocalChanAlias RPC.
			err = s.cfg.AliasMgr.AddLocalAlias(
				aliasScid, baseScid, false, true,
				aliasmgr.WithBaseLookup(),
			)
			if err != nil {
				return nil, fmt.Errorf("error adding scid "+
					"alias, base_scid=%v, alias_scid=%v: "+
					"%w", baseScid, aliasScid, err)
			}
		}
	}

	return &AddAliasesResponse{
		AliasMaps: lnrpc.MarshalAliasMap(s.cfg.AliasMgr.ListAliases()),
	}, nil
}

// XDeleteLocalChanAliases is an experimental API that deletes a set of alias
// mappings. The final total set of aliases in the manager after the delete
// operation is returned. The deletion will not be communicated to the channel
// peer via any message.
func (s *Server) XDeleteLocalChanAliases(_ context.Context,
	in *DeleteAliasesRequest) (*DeleteAliasesResponse,
	error) {

	for _, v := range in.AliasMaps {
		baseScid := lnwire.NewShortChanIDFromInt(v.BaseScid)

		for _, alias := range v.Aliases {
			aliasScid := lnwire.NewShortChanIDFromInt(alias)

			err := s.cfg.AliasMgr.DeleteLocalAlias(
				aliasScid, baseScid,
			)
			if err != nil {
				return nil, fmt.Errorf("error deleting scid "+
					"alias, base_scid=%v, alias_scid=%v: "+
					"%w", baseScid, aliasScid, err)
			}
		}
	}

	return &DeleteAliasesResponse{
		AliasMaps: lnrpc.MarshalAliasMap(s.cfg.AliasMgr.ListAliases()),
	}, nil
}

// XFindBaseLocalChanAlias is an experimental API that looks up the base scid
// for a local chan alias that was registered.
func (s *Server) XFindBaseLocalChanAlias(_ context.Context,
	in *FindBaseAliasRequest) (*FindBaseAliasResponse, error) {

	aliasScid := lnwire.NewShortChanIDFromInt(in.Alias)
	base, err := s.cfg.AliasMgr.FindBaseSCID(aliasScid)
	if err != nil {
		return nil, err
	}

	return &FindBaseAliasResponse{
		Base: base.ToUint64(),
	}, nil
}

func extractOutPoint(req *UpdateChanStatusRequest) (*wire.OutPoint, error) {
	chanPoint := req.GetChanPoint()
	txid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		return nil, err
	}
	index := chanPoint.OutputIndex
	return wire.NewOutPoint(txid, index), nil
}

// UpdateChanStatus allows channel state to be set manually.
func (s *Server) UpdateChanStatus(_ context.Context,
	req *UpdateChanStatusRequest) (*UpdateChanStatusResponse, error) {

	outPoint, err := extractOutPoint(req)
	if err != nil {
		return nil, err
	}

	action := req.GetAction()

	log.Debugf("UpdateChanStatus called for channel(%v) with "+
		"action %v", outPoint, action)

	switch action {
	case ChanStatusAction_ENABLE:
		err = s.cfg.RouterBackend.SetChannelEnabled(*outPoint)
	case ChanStatusAction_DISABLE:
		err = s.cfg.RouterBackend.SetChannelDisabled(*outPoint)
	case ChanStatusAction_AUTO:
		err = s.cfg.RouterBackend.SetChannelAuto(*outPoint)
	default:
		err = fmt.Errorf("unrecognized ChannelStatusAction %v", action)
	}

	if err != nil {
		return nil, err
	}
	return &UpdateChanStatusResponse{}, nil
}
