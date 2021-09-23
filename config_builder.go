package lnd

import (
	"context"
	"fmt"

	proxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// GrpcRegistrar is an interface that must be satisfied by an external subserver
// that wants to be able to register its own gRPC server onto lnd's main
// grpc.Server instance.
type GrpcRegistrar interface {
	// RegisterGrpcSubserver is called for each net.Listener on which lnd
	// creates a grpc.Server instance. External subservers implementing this
	// method can then register their own gRPC server structs to the main
	// server instance.
	RegisterGrpcSubserver(*grpc.Server) error
}

// RestRegistrar is an interface that must be satisfied by an external subserver
// that wants to be able to register its own REST mux onto lnd's main
// proxy.ServeMux instance.
type RestRegistrar interface {
	// RegisterRestSubserver is called after lnd creates the main
	// proxy.ServeMux instance. External subservers implementing this method
	// can then register their own REST proxy stubs to the main server
	// instance.
	RegisterRestSubserver(context.Context, *proxy.ServeMux, string,
		[]grpc.DialOption) error
}

// ExternalValidator is an interface that must be satisfied by an external
// macaroon validator.
type ExternalValidator interface {
	macaroons.MacaroonValidator

	// Permissions returns the permissions that the external validator is
	// validating. It is a map between the full HTTP URI of each RPC and its
	// required macaroon permissions. If multiple action/entity tuples are
	// specified per URI, they are all required. See rpcserver.go for a list
	// of valid action and entity values.
	Permissions() map[string][]bakery.Op
}

// ImplementationCfg is a struct that holds all configuration items for
// components that can be implemented outside lnd itself.
type ImplementationCfg struct {
	// GrpcRegistrar is a type that can register additional gRPC subservers
	// before the main gRPC server is started.
	GrpcRegistrar

	// RestRegistrar is a type that can register additional REST subservers
	// before the main REST proxy is started.
	RestRegistrar

	// ExternalValidator is a type that can provide external macaroon
	// validation.
	ExternalValidator
}

// DefaultWalletImpl is the default implementation of our normal, btcwallet
// backed configuration.
type DefaultWalletImpl struct {
}

// RegisterRestSubserver is called after lnd creates the main proxy.ServeMux
// instance. External subservers implementing this method can then register
// their own REST proxy stubs to the main server instance.
//
// NOTE: This is part of the GrpcRegistrar interface.
func (d *DefaultWalletImpl) RegisterRestSubserver(context.Context,
	*proxy.ServeMux, string, []grpc.DialOption) error {

	return nil
}

// RegisterGrpcSubserver is called for each net.Listener on which lnd creates a
// grpc.Server instance. External subservers implementing this method can then
// register their own gRPC server structs to the main server instance.
//
// NOTE: This is part of the GrpcRegistrar interface.
func (d *DefaultWalletImpl) RegisterGrpcSubserver(*grpc.Server) error {
	return nil
}

// ValidateMacaroon extracts the macaroon from the context's gRPC metadata,
// checks its signature, makes sure all specified permissions for the called
// method are contained within and finally ensures all caveat conditions are
// met. A non-nil error is returned if any of the checks fail.
//
// NOTE: This is part of the ExternalValidator interface.
func (d *DefaultWalletImpl) ValidateMacaroon(ctx context.Context,
	requiredPermissions []bakery.Op, fullMethod string) error {

	// Because the default implementation does not return any permissions,
	// we shouldn't be registered as an external validator at all and this
	// should never be invoked.
	return fmt.Errorf("default implementation does not support external " +
		"macaroon validation")
}

// Permissions returns the permissions that the external validator is
// validating. It is a map between the full HTTP URI of each RPC and its
// required macaroon permissions. If multiple action/entity tuples are specified
// per URI, they are all required. See rpcserver.go for a list of valid action
// and entity values.
//
// NOTE: This is part of the ExternalValidator interface.
func (d *DefaultWalletImpl) Permissions() map[string][]bakery.Op {
	return nil
}
