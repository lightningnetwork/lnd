//go:build peersrpc
// +build peersrpc

package peersrpc

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize tt as the name of our
	// RPC service.
	subServerName = "PeersRPC"
)

var (
	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/peersrpc.Peers/UpdateNodeAnnouncement": {{
			Entity: "peers",
			Action: "write",
		}},
	}
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	PeersServer
}

// Server is a sub-server of the main RPC server: the peers RPC. This sub
// RPC server allows to intereact with our Peers in the Lightning Network.
type Server struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	UnimplementedPeersServer

	cfg *Config
}

// A compile time check to ensure that Server fully implements the PeersServer
// gRPC service.
var _ PeersServer = (*Server)(nil)

// New returns a new instance of the peersrpc Peers sub-server. We also
// return the set of permissions for the macaroons that we may create within
// this method. If the macaroons we need aren't found in the filepath, then
// we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	server := &Server{
		cfg: cfg,
	}

	return server, macPermissions, nil
}

// Start launches any helper goroutines required for the Server to function.
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
	RegisterPeersServer(grpcServer, r)

	log.Debugf("Peers RPC server successfully registered with root " +
		"gRPC server")

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
	err := RegisterPeersHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Peers REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("Peers REST server successfully registered with " +
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

	r.PeersServer = subServer
	return subServer, macPermissions, nil
}

// updateAddresses computes the new address set after executing the update
// actions.
func (s *Server) updateAddresses(currentAddresses []net.Addr,
	updates []*UpdateAddressAction) ([]net.Addr, *lnrpc.Op, error) {

	// net.Addr is not comparable so we cannot use the default map
	// (map[net.Addr]struct{}) so we have to use arrays and a helping
	// function.
	findAddr := func(addr net.Addr, slice []net.Addr) bool {
		for _, sAddr := range slice {
			if sAddr.Network() != addr.Network() {
				continue
			}

			if sAddr.String() == addr.String() {
				return true
			}
		}
		return false
	}

	// Preallocate enough memory for both arrays.
	removeAddr := make([]net.Addr, 0, len(updates))
	addAddr := make([]net.Addr, 0, len(updates))
	for _, update := range updates {
		addr, err := s.cfg.ParseAddr(update.Address)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to resolve "+
				"address %v: %v", update.Address, err)
		}

		switch update.Action {
		case UpdateAction_ADD:
			addAddr = append(addAddr, addr)
		case UpdateAction_REMOVE:
			removeAddr = append(removeAddr, addr)
		default:
			return nil, nil, fmt.Errorf("invalid address update "+
				"action: %v", update.Action)
		}
	}

	// Look for any inconsistency trying to add AND remove the same address.
	for _, addr := range removeAddr {
		if findAddr(addr, addAddr) {
			return nil, nil, fmt.Errorf("invalid updates for "+
				"removing AND adding %v", addr)
		}
	}

	ops := &lnrpc.Op{Entity: "addresses"}
	newAddrs := make([]net.Addr, 0, len(updates)+len(currentAddresses))

	// Copy current addresses excluding the ones that need to be removed.
	for _, addr := range currentAddresses {
		if findAddr(addr, removeAddr) {
			ops.Actions = append(
				ops.Actions,
				fmt.Sprintf("%s removed", addr.String()),
			)
			continue
		}
		newAddrs = append(newAddrs, addr)
	}

	// Add new adresses if needed.
	for _, addr := range addAddr {
		if !findAddr(addr, newAddrs) {
			ops.Actions = append(
				ops.Actions,
				fmt.Sprintf("%s added", addr.String()),
			)
			newAddrs = append(newAddrs, addr)
		}
	}

	return newAddrs, ops, nil
}

// updateFeatures computes the new raw SetNodeAnn after executing the update
// actions.
func (s *Server) updateFeatures(currentfeatures *lnwire.RawFeatureVector,
	updates []*UpdateFeatureAction) (*lnwire.RawFeatureVector,
	*lnrpc.Op, error) {

	ops := &lnrpc.Op{Entity: "features"}
	raw := currentfeatures.Clone()

	for _, update := range updates {
		bit := lnwire.FeatureBit(update.FeatureBit)

		switch update.Action {
		case UpdateAction_ADD:
			if raw.IsSet(bit) {
				return nil, nil, fmt.Errorf(
					"invalid add action for bit %v, "+
						"bit is already set",
					update.FeatureBit,
				)
			}
			raw.Set(bit)
			ops.Actions = append(
				ops.Actions,
				fmt.Sprintf("%s set", lnwire.Features[bit]),
			)

		case UpdateAction_REMOVE:
			if !raw.IsSet(bit) {
				return nil, nil, fmt.Errorf(
					"invalid remove action for bit %v, "+
						"bit is already unset",
					update.FeatureBit,
				)
			}
			raw.Unset(bit)
			ops.Actions = append(
				ops.Actions,
				fmt.Sprintf("%s unset", lnwire.Features[bit]),
			)

		default:
			return nil, nil, fmt.Errorf(
				"invalid update action (%v) for bit %v",
				update.Action,
				update.FeatureBit,
			)
		}
	}

	// Validate our new SetNodeAnn.
	fv := lnwire.NewFeatureVector(raw, lnwire.Features)
	if err := feature.ValidateDeps(fv); err != nil {
		return nil, nil, fmt.Errorf(
			"invalid feature set (SetNodeAnn): %v",
			err,
		)
	}

	return raw, ops, nil
}

// UpdateNodeAnnouncement allows the caller to update the node parameters
// and broadcasts a new version of the node announcement to its peers.
func (s *Server) UpdateNodeAnnouncement(ctx context.Context,
	req *NodeAnnouncementUpdateRequest) (
	*NodeAnnouncementUpdateResponse, error) {

	resp := &NodeAnnouncementUpdateResponse{}
	nodeModifiers := make([]netann.NodeAnnModifier, 0)

	currentNodeAnn := s.cfg.GetNodeAnnouncement()

	nodeAnnFeatures := currentNodeAnn.Features
	featureUpdates := len(req.FeatureUpdates) > 0
	if featureUpdates {
		var (
			ops *lnrpc.Op
			err error
		)
		nodeAnnFeatures, ops, err = s.updateFeatures(
			nodeAnnFeatures, req.FeatureUpdates,
		)
		if err != nil {
			return nil, fmt.Errorf("error trying to update node "+
				"features: %w", err)
		}
		resp.Ops = append(resp.Ops, ops)
	}

	if req.Color != "" {
		color, err := lncfg.ParseHexColor(req.Color)
		if err != nil {
			return nil, fmt.Errorf("unable to parse color: %w", err)
		}

		if color != currentNodeAnn.RGBColor {
			resp.Ops = append(resp.Ops, &lnrpc.Op{
				Entity: "color",
				Actions: []string{
					fmt.Sprintf("changed to %v", color),
				},
			})
			nodeModifiers = append(
				nodeModifiers,
				netann.NodeAnnSetColor(color),
			)
		}
	}

	if req.Alias != "" {
		alias, err := lnwire.NewNodeAlias(req.Alias)
		if err != nil {
			return nil, fmt.Errorf("invalid alias value: %w", err)
		}
		if alias != currentNodeAnn.Alias {
			resp.Ops = append(resp.Ops, &lnrpc.Op{
				Entity: "alias",
				Actions: []string{
					fmt.Sprintf("changed to %v", alias),
				},
			})
			nodeModifiers = append(
				nodeModifiers,
				netann.NodeAnnSetAlias(alias),
			)
		}
	}

	if len(req.AddressUpdates) > 0 {
		newAddrs, ops, err := s.updateAddresses(
			currentNodeAnn.Addresses,
			req.AddressUpdates,
		)
		if err != nil {
			return nil, fmt.Errorf("error trying to update node "+
				"addresses: %w", err)
		}
		resp.Ops = append(resp.Ops, ops)
		nodeModifiers = append(
			nodeModifiers,
			netann.NodeAnnSetAddrs(newAddrs),
		)
	}

	if len(nodeModifiers) == 0 && !featureUpdates {
		return nil, fmt.Errorf("unable to detect any new values to " +
			"update the node announcement")
	}

	if err := s.cfg.UpdateNodeAnnouncement(
		ctx, nodeAnnFeatures, nodeModifiers...,
	); err != nil {
		return nil, err
	}

	return resp, nil
}
