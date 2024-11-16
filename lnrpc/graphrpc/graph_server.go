//go:build graphrpc
// +build graphrpc

package graphrpc

import (
	"context"
	"encoding/hex"
	"sync/atomic"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/graph/sources"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize it as the name of our
	// RPC service.
	subServerName = "GraphRPC"
)

var (
	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/graphrpc.Graph/BetweennessCentrality": {{
			Entity: "graph",
			Action: "read",
		}},
		"/graphrpc.Graph/BootstrapperName": {{
			Entity: "graph",
			Action: "read",
		}},
		"/graphrpc.Graph/BootstrapAddrs": {{
			Entity: "graph",
			Action: "read",
		}},
	}
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	GraphServer
}

// Server is a sub-server of the main RPC server: the graph RPC.
type Server struct {
	started  atomic.Bool
	shutdown atomic.Bool

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	UnimplementedGraphServer

	cfg *Config

	chanGraph    *sources.DBSource
	bootstrapper discovery.NetworkPeerBootstrapper
}

// A compile-time check to ensure that Server fully implements the GraphServer
// gRPC service.
var _ GraphServer = (*Server)(nil)

// New returns a new instance of the graphrpc Graph sub-server. We also
// return the set of permissions for the macaroons that we may create within
// this method. If the macaroons we need aren't found in the filepath, then
// we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	chanGraph := autopilot.ChannelGraphFromDatabase(cfg.GraphDB)

	bootstrapper, err := discovery.NewGraphBootstrapper(chanGraph)
	if err != nil {
		return nil, nil, err
	}

	server := &Server{
		cfg:          cfg,
		bootstrapper: bootstrapper,
		chanGraph:    sources.NewDBGSource(cfg.GraphDB),
	}

	return server, macPermissions, nil
}

// Start launches any helper goroutines required for the Server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	if !s.started.CompareAndSwap(false, true) {
		return nil
	}

	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	if !s.shutdown.CompareAndSwap(false, true) {
		return nil
	}

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
// is called, each sub-server won't be able to have
// requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterGraphServer(grpcServer, r)

	log.Debugf("Graph RPC server successfully register with root gRPC " +
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
	err := RegisterGraphHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Graph REST server "+
			"with root REST server: %v", err)

		return err
	}

	log.Debugf("Graph REST server successfully registered with " +
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
func (r *ServerShell) CreateSubServer(
	configRegistry lnrpc.SubServerConfigDispatcher) (lnrpc.SubServer,
	lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.GraphServer = subServer

	return subServer, macPermissions, nil
}

// BetweennessCentrality computes the normalised and non-normalised betweenness
// centrality for each node in the graph.
func (s *Server) BetweennessCentrality(ctx context.Context,
	_ *BetweennessCentralityReq) (*BetweennessCentralityResp, error) {

	res, err := s.chanGraph.BetweennessCentrality(ctx)
	if err != nil {
		return nil, err
	}

	nodeBetweenness := make([]*BetweennessCentrality, 0, len(res))
	for nodeID, val := range res {
		nodeBetweenness = append(
			nodeBetweenness, &BetweennessCentrality{
				Node:          nodeID[:],
				Normalized:    val.Normalized,
				NonNormalized: val.NonNormalized,
			},
		)
	}

	return &BetweennessCentralityResp{
		NodeBetweenness: nodeBetweenness,
	}, nil
}

// BootstrapperName returns the name of the bootstrapper implementation being
// used by the Server.
func (s *Server) BootstrapperName(ctx context.Context, _ *BoostrapperNameReq) (
	*BoostrapperNameResp, error) {

	return &BoostrapperNameResp{
		Name: s.bootstrapper.Name(ctx),
	}, nil
}

// BootstrapAddrs uniformly samples a set of specified address from backing
// bootstrapper source. NumAddrs can be used to denote how many valid peer
// addresses to return. IgnoreNodes can be used to specify a set of nodes to
// exclude from the result list.
func (s *Server) BootstrapAddrs(ctx context.Context, req *BootstrapAddrsReq) (
	*BootstrapAddrsResp, error) {

	ignore := make(map[autopilot.NodeID]struct{})
	for _, addr := range req.IgnoreNodes {
		var id autopilot.NodeID
		copy(id[:], addr)

		ignore[id] = struct{}{}
	}

	addrs, err := s.bootstrapper.SampleNodeAddrs(ctx, req.NumAddrs, ignore)
	if err != nil {
		return nil, err
	}

	netAddrs := make(map[string]*Addresses)
	for _, addr := range addrs {
		nodeID := hex.EncodeToString(
			addr.IdentityKey.SerializeCompressed(),
		)

		if _, ok := netAddrs[nodeID]; !ok {
			netAddrs[nodeID] = &Addresses{}
		}

		netAddrs[nodeID].Addresses = append(
			netAddrs[nodeID].Addresses, &lnrpc.NodeAddress{
				Addr:    addr.Address.String(),
				Network: addr.Network(),
			},
		)
	}

	return &BootstrapAddrsResp{Addresses: netAddrs}, nil
}
