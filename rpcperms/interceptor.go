package rpcperms

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btclog"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/monitoring"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// rpcState is an enum that we use to keep track of the current RPC service
// state. This will transition as we go from startup to unlocking the wallet,
// and finally fully active.
type rpcState uint8

const (
	// walletNotCreated is the starting state if the RPC server is active,
	// but the wallet is not yet created. In this state we'll only allow
	// calls to the WalletUnlockerService.
	walletNotCreated rpcState = iota

	// walletLocked indicates the RPC server is active, but the wallet is
	// locked. In this state we'll only allow calls to the
	// WalletUnlockerService.
	walletLocked

	// walletUnlocked means that the wallet has been unlocked, but the full
	// RPC server is not yeat ready.
	walletUnlocked

	// rpcActive means that the RPC server is ready to accept calls.
	rpcActive
)

var (
	// ErrNoWallet is returned if the wallet does not exist.
	ErrNoWallet = fmt.Errorf("wallet not created, create one to enable " +
		"full RPC access")

	// ErrWalletLocked is returned if the wallet is locked and any service
	// other than the WalletUnlocker is called.
	ErrWalletLocked = fmt.Errorf("wallet locked, unlock it to enable " +
		"full RPC access")

	// ErrWalletUnlocked is returned if the WalletUnlocker service is
	// called when the wallet already has been unlocked.
	ErrWalletUnlocked = fmt.Errorf("wallet already unlocked, " +
		"WalletUnlocker service is no longer available")

	// ErrRPCStarting is returned if the wallet has been unlocked but the
	// RPC server is not yet ready to accept calls.
	ErrRPCStarting = fmt.Errorf("the RPC server is in the process of " +
		"starting up, but not yet ready to accept calls")

	// macaroonWhitelist defines methods that we don't require macaroons to
	// access.
	macaroonWhitelist = map[string]struct{}{
		// We allow all calls to the WalletUnlocker without macaroons.
		"/lnrpc.WalletUnlocker/GenSeed":        {},
		"/lnrpc.WalletUnlocker/InitWallet":     {},
		"/lnrpc.WalletUnlocker/UnlockWallet":   {},
		"/lnrpc.WalletUnlocker/ChangePassword": {},
	}
)

// InterceptorChain is a struct that can be added to the running GRPC server,
// intercepting API calls. This is useful for logging, enforcing permissions
// etc.
type InterceptorChain struct {
	// state is the current RPC state of our RPC server.
	state rpcState

	// noMacaroons should be set true if we don't want to check macaroons.
	noMacaroons bool

	// svc is the macaroon service used to enforce permissions in case
	// macaroons are used.
	svc *macaroons.Service

	// permissionMap is the permissions to enforce if macaroons are used.
	permissionMap map[string][]bakery.Op

	// rpcsLog is the logger used to log calles to the RPCs intercepted.
	rpcsLog btclog.Logger

	sync.RWMutex
}

// NewInterceptorChain creates a new InterceptorChain.
func NewInterceptorChain(log btclog.Logger, noMacaroons,
	walletExists bool) *InterceptorChain {

	startState := walletNotCreated
	if walletExists {
		startState = walletLocked
	}

	return &InterceptorChain{
		state:         startState,
		noMacaroons:   noMacaroons,
		permissionMap: make(map[string][]bakery.Op),
		rpcsLog:       log,
	}
}

// SetWalletUnlocked moves the RPC state from either walletNotCreated or
// walletLocked to walletUnlocked.
func (r *InterceptorChain) SetWalletUnlocked() {
	r.Lock()
	defer r.Unlock()

	r.state = walletUnlocked
}

// SetRPCActive moves the RPC state from walletUnlocked to rpcActive.
func (r *InterceptorChain) SetRPCActive() {
	r.Lock()
	defer r.Unlock()

	r.state = rpcActive
}

// AddMacaroonService adds a macaroon service to the interceptor. After this is
// done every RPC call made will have to pass a valid macaroon to be accepted.
func (r *InterceptorChain) AddMacaroonService(svc *macaroons.Service) {
	r.Lock()
	defer r.Unlock()

	r.svc = svc
}

// AddPermission adds a new macaroon rule for the given method.
func (r *InterceptorChain) AddPermission(method string, ops []bakery.Op) error {
	r.Lock()
	defer r.Unlock()

	if _, ok := r.permissionMap[method]; ok {
		return fmt.Errorf("detected duplicate macaroon constraints "+
			"for path: %v", method)
	}

	r.permissionMap[method] = ops
	return nil
}

// Permissions returns the current set of macaroon permissions.
func (r *InterceptorChain) Permissions() map[string][]bakery.Op {
	r.RLock()
	defer r.RUnlock()

	// Make a copy under the read lock to avoid races.
	c := make(map[string][]bakery.Op)
	for k, v := range r.permissionMap {
		s := make([]bakery.Op, len(v))
		copy(s, v)
		c[k] = s
	}

	return c
}

// CreateServerOpts creates the GRPC server options that can be added to a GRPC
// server in order to add this InterceptorChain.
func (r *InterceptorChain) CreateServerOpts() []grpc.ServerOption {
	var unaryInterceptors []grpc.UnaryServerInterceptor
	var strmInterceptors []grpc.StreamServerInterceptor

	// The first interceptors we'll add to the chain is our logging
	// interceptors, so we can automatically log all errors that happen
	// during RPC calls.
	unaryInterceptors = append(
		unaryInterceptors, errorLogUnaryServerInterceptor(r.rpcsLog),
	)
	strmInterceptors = append(
		strmInterceptors, errorLogStreamServerInterceptor(r.rpcsLog),
	)

	// Next we'll add our RPC state check interceptors, that will check
	// whether the attempted call is allowed in the current state.
	unaryInterceptors = append(
		unaryInterceptors, r.rpcStateUnaryServerInterceptor(),
	)
	strmInterceptors = append(
		strmInterceptors, r.rpcStateStreamServerInterceptor(),
	)

	// We'll add the macaroon interceptors. If macaroons aren't disabled,
	// then these interceptors will enforce macaroon authentication.
	unaryInterceptors = append(
		unaryInterceptors, r.MacaroonUnaryServerInterceptor(),
	)
	strmInterceptors = append(
		strmInterceptors, r.MacaroonStreamServerInterceptor(),
	)

	// Get interceptors for Prometheus to gather gRPC performance metrics.
	// If monitoring is disabled, GetPromInterceptors() will return empty
	// slices.
	promUnaryInterceptors, promStrmInterceptors :=
		monitoring.GetPromInterceptors()

	// Concatenate the slices of unary and stream interceptors respectively.
	unaryInterceptors = append(unaryInterceptors, promUnaryInterceptors...)
	strmInterceptors = append(strmInterceptors, promStrmInterceptors...)

	// Create server options from the interceptors we just set up.
	chainedUnary := grpc_middleware.WithUnaryServerChain(
		unaryInterceptors...,
	)
	chainedStream := grpc_middleware.WithStreamServerChain(
		strmInterceptors...,
	)
	serverOpts := []grpc.ServerOption{chainedUnary, chainedStream}

	return serverOpts
}

// errorLogUnaryServerInterceptor is a simple UnaryServerInterceptor that will
// automatically log any errors that occur when serving a client's unary
// request.
func errorLogUnaryServerInterceptor(logger btclog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		resp, err := handler(ctx, req)
		if err != nil {
			// TODO(roasbeef): also log request details?
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return resp, err
	}
}

// errorLogStreamServerInterceptor is a simple StreamServerInterceptor that
// will log any errors that occur while processing a client or server streaming
// RPC.
func errorLogStreamServerInterceptor(logger btclog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		err := handler(srv, ss)
		if err != nil {
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return err
	}
}

// checkMacaroon validates that the context contains the macaroon needed to
// invoke the given RPC method.
func (r *InterceptorChain) checkMacaroon(ctx context.Context,
	fullMethod string) error {

	// If noMacaroons is set, we'll always allow the call.
	if r.noMacaroons {
		return nil
	}

	// Check whether the method is whitelisted, if so we'll allow it
	// regardless of macaroons.
	_, ok := macaroonWhitelist[fullMethod]
	if ok {
		return nil
	}

	r.RLock()
	svc := r.svc
	r.RUnlock()

	// If the macaroon service is not yet active, we cannot allow
	// the call.
	if svc == nil {
		return fmt.Errorf("unable to determine macaroon permissions")
	}

	r.RLock()
	uriPermissions, ok := r.permissionMap[fullMethod]
	r.RUnlock()
	if !ok {
		return fmt.Errorf("%s: unknown permissions required for method",
			fullMethod)
	}

	// Find out if there is an external validator registered for
	// this method. Fall back to the internal one if there isn't.
	validator, ok := svc.ExternalValidators[fullMethod]
	if !ok {
		validator = svc
	}

	// Now that we know what validator to use, let it do its work.
	return validator.ValidateMacaroon(ctx, uriPermissions, fullMethod)
}

// MacaroonUnaryServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (r *InterceptorChain) MacaroonUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		// Check macaroons.
		if err := r.checkMacaroon(ctx, info.FullMethod); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// MacaroonStreamServerInterceptor is a GRPC interceptor that checks whether
// the request is authorized by the included macaroons.
func (r *InterceptorChain) MacaroonStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		// Check macaroons.
		err := r.checkMacaroon(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}

		return handler(srv, ss)
	}
}

// checkRPCState checks whether a call to the given server is allowed in the
// current RPC state.
func (r *InterceptorChain) checkRPCState(srv interface{}) error {
	r.RLock()
	state := r.state
	r.RUnlock()

	switch state {

	// If the wallet does not exists, only calls to the WalletUnlocker are
	// accepted.
	case walletNotCreated:
		_, ok := srv.(lnrpc.WalletUnlockerServer)
		if !ok {
			return ErrNoWallet
		}

	// If the wallet is locked, only calls to the WalletUnlocker are
	// accepted.
	case walletLocked:
		_, ok := srv.(lnrpc.WalletUnlockerServer)
		if !ok {
			return ErrWalletLocked
		}

	// If the wallet is unlocked, but the RPC not yet active, we reject.
	case walletUnlocked:
		_, ok := srv.(lnrpc.WalletUnlockerServer)
		if ok {
			return ErrWalletUnlocked
		}

		return ErrRPCStarting

	// If the RPC is active, we allow calls to any service except the
	// WalletUnlocker.
	case rpcActive:
		_, ok := srv.(lnrpc.WalletUnlockerServer)
		if ok {
			return ErrWalletUnlocked
		}

	default:
		return fmt.Errorf("unknown RPC state: %v", state)
	}

	return nil
}

// rpcStateUnaryServerInterceptor is a GRPC interceptor that checks whether
// calls to the given gGRPC server is allowed in the current rpc state.
func (r *InterceptorChain) rpcStateUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		if err := r.checkRPCState(info.Server); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// rpcStateStreamServerInterceptor is a GRPC interceptor that checks whether
// calls to the given gGRPC server is allowed in the current rpc state.
func (r *InterceptorChain) rpcStateStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		if err := r.checkRPCState(srv); err != nil {
			return err
		}

		return handler(srv, ss)
	}
}
