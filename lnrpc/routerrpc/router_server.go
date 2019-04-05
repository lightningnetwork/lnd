// +build routerrpc

package routerrpc

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc"
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
			Action: "write",
		},
	}

	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/routerpc.Router/SendPayment": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/routerpc.Router/EstimateRouteFee": {{
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
func (s *Server) SendPayment(ctx context.Context,
	req *PaymentRequest) (*PaymentResponse, error) {

	switch {
	// If the payment request isn't populated, then we won't be able to
	// even attempt a payment.
	case req.PayReq == "":
		return nil, fmt.Errorf("a valid payment request MUST be specified")
	}

	// Now that we know the payment request is present, we'll attempt to
	// decode it in order to parse out all the parameters for the route.
	payReq, err := zpay32.Decode(req.PayReq, s.cfg.ActiveNetParams)
	if err != nil {
		return nil, err
	}

	// Atm, this service does not support invoices that don't have their
	// value fully specified.
	if payReq.MilliSat == nil {
		return nil, fmt.Errorf("zero value invoices are not supported")
	}

	var destination route.Vertex
	copy(destination[:], payReq.Destination.SerializeCompressed())

	// Now that all the information we need has been parsed, we'll map this
	// proto request into a proper request that our backing router can
	// understand.
	finalDelta := uint16(payReq.MinFinalCLTVExpiry())
	payment := routing.LightningPayment{
		Target:            destination,
		Amount:            *payReq.MilliSat,
		FeeLimit:          lnwire.MilliSatoshi(req.FeeLimitSat),
		PaymentHash:       *payReq.PaymentHash,
		FinalCLTVDelta:    &finalDelta,
		PayAttemptTimeout: time.Second * time.Duration(req.TimeoutSeconds),
		RouteHints:        payReq.RouteHints,
	}

	// Pin to an outgoing channel if specified.
	if req.OutgoingChannelId != 0 {
		chanID := uint64(req.OutgoingChannelId)
		payment.OutgoingChannelID = &chanID
	}

	preImage, _, err := s.cfg.Router.SendPayment(&payment)
	if err != nil {
		return nil, err
	}

	return &PaymentResponse{
		PayHash:  (*payReq.PaymentHash)[:],
		PreImage: preImage[:],
	}, nil
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
	// that target amount, we'll only request a single route.
	routes, err := s.cfg.Router.FindRoutes(
		s.cfg.RouterBackend.SelfNode, destNode, amtMsat,
		&routing.RestrictParams{
			FeeLimit: feeLimit,
		}, 1,
	)
	if err != nil {
		return nil, err
	}

	if len(routes) == 0 {
		return nil, fmt.Errorf("unable to find route to dest: %v", err)
	}

	return &RouteFeeResponse{
		RoutingFeeMsat: int64(routes[0].TotalFees),
		TimeLockDelay:  int64(routes[0].TotalTimeLock),
	}, nil
}
