package rpcperms

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/monitoring"
	"github.com/lightningnetwork/lnd/subscribe"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// rpcState is an enum that we use to keep track of the current RPC service
// state. This will transition as we go from startup to unlocking the wallet,
// and finally fully active.
type rpcState uint8

const (
	// waitingToStart indicates that we're at the beginning of the startup
	// process. In a cluster environment this may mean that we're waiting to
	// become the leader in which case RPC calls will be disabled until
	// this instance has been elected as leader.
	waitingToStart rpcState = iota

	// walletNotCreated is the starting state if the RPC server is active,
	// but the wallet is not yet created. In this state we'll only allow
	// calls to the WalletUnlockerService.
	walletNotCreated

	// walletLocked indicates the RPC server is active, but the wallet is
	// locked. In this state we'll only allow calls to the
	// WalletUnlockerService.
	walletLocked

	// walletUnlocked means that the wallet has been unlocked, but the full
	// RPC server is not yet ready.
	walletUnlocked

	// rpcActive means that the RPC server is ready to accept calls.
	rpcActive

	// serverActive means that the lnd server is ready to accept calls.
	serverActive
)

var (
	// ErrWaitingToStart is returned if LND is still waiting to start,
	// possibly blocked until elected as the leader.
	ErrWaitingToStart = fmt.Errorf("waiting to start, RPC services not " +
		"available")

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
	// access. We also allow these methods to be called even if not all
	// mandatory middlewares are registered yet. If the wallet is locked
	// then a middleware cannot register itself, creating an impossible
	// situation. Also, a middleware might want to check the state of lnd
	// by calling the State service before it registers itself. So we also
	// need to exclude those calls from the mandatory middleware check.
	macaroonWhitelist = map[string]struct{}{
		// We allow all calls to the WalletUnlocker without macaroons.
		"/lnrpc.WalletUnlocker/GenSeed":        {},
		"/lnrpc.WalletUnlocker/InitWallet":     {},
		"/lnrpc.WalletUnlocker/UnlockWallet":   {},
		"/lnrpc.WalletUnlocker/ChangePassword": {},

		// The State service must be available at all times, even
		// before we can check macaroons, so we whitelist it.
		"/lnrpc.State/SubscribeState": {},
		"/lnrpc.State/GetState":       {},
	}
)

// InterceptorChain is a struct that can be added to the running GRPC server,
// intercepting API calls. This is useful for logging, enforcing permissions,
// supporting middleware etc. The following diagram shows the order of each
// interceptor in the chain and when exactly requests/responses are intercepted
// and forwarded to external middleware for approval/modification. Middleware in
// general can only intercept gRPC requests/responses that are sent by the
// client with a macaroon that contains a custom caveat that is supported by one
// of the registered middlewares.
//
//	    |
//	    | gRPC request from client
//	    |
//	+---v--------------------------------+
//	|   InterceptorChain                 |
//	+-+----------------------------------+
//	  | Log Interceptor                  |
//	  +----------------------------------+
//	  | RPC State Interceptor            |
//	  +----------------------------------+
//	  | Macaroon Interceptor             |
//	  +----------------------------------+--------> +---------------------+
//	  | RPC Macaroon Middleware Handler  |<-------- | External Middleware |
//	  +----------------------------------+          |   - modify request |
//	  | Prometheus Interceptor           |          +---------------------+
//	  +-+--------------------------------+
//	    | validated gRPC request from client
//	+---v--------------------------------+
//	|   main gRPC server                 |
//	+---+--------------------------------+
//	    |
//	    | original gRPC request to client
//	    |
//	+---v--------------------------------+--------> +---------------------+
//	|   RPC Macaroon Middleware Handler  |<-------- | External Middleware |
//	+---+--------------------------------+          |   - modify response |
//	    |                                           +---------------------+
//	    | edited gRPC request to client
//	    v
type InterceptorChain struct {
	// lastRequestID is the ID of the last gRPC request or stream that was
	// intercepted by the middleware interceptor.
	//
	// NOTE: Must be used atomically!
	lastRequestID uint64

	// Required by the grpc-gateway/v2 library for forward compatibility.
	lnrpc.UnimplementedStateServer

	started sync.Once
	stopped sync.Once

	// state is the current RPC state of our RPC server.
	state rpcState

	// ntfnServer is a subscription server we use to notify clients of the
	// State service when the state changes.
	ntfnServer *subscribe.Server

	// noMacaroons should be set true if we don't want to check macaroons.
	noMacaroons bool

	// svc is the macaroon service used to enforce permissions in case
	// macaroons are used.
	svc *macaroons.Service

	// permissionMap is the permissions to enforce if macaroons are used.
	permissionMap map[string][]bakery.Op

	// rpcsLog is the logger used to log calls to the RPCs intercepted.
	rpcsLog btclog.Logger

	// registeredMiddleware is a slice of all macaroon permission based RPC
	// middleware clients that are currently registered. The
	// registeredMiddlewareNames can be used to find the index of a specific
	// interceptor within the registeredMiddleware slide using the name of
	// the interceptor as the key. The reason for using these two separate
	// structures is so that the order in which interceptors are run is
	// the same as the order in which they were registered.
	registeredMiddleware []*MiddlewareHandler

	// registeredMiddlewareNames is a map of registered middleware names
	// to the index at which they are stored in the registeredMiddleware
	// map.
	registeredMiddlewareNames map[string]int

	// mandatoryMiddleware is a list of all middleware that is considered to
	// be mandatory. If any of them is not registered then all RPC requests
	// (except for the macaroon white listed methods and the middleware
	// registration itself) are blocked. This is a security feature to make
	// sure that requests can't just go through unobserved/unaudited if a
	// middleware crashes.
	mandatoryMiddleware []string

	quit chan struct{}
	sync.RWMutex
}

// A compile time check to ensure that InterceptorChain fully implements the
// StateServer gRPC service.
var _ lnrpc.StateServer = (*InterceptorChain)(nil)

// NewInterceptorChain creates a new InterceptorChain.
func NewInterceptorChain(log btclog.Logger, noMacaroons bool,
	mandatoryMiddleware []string) *InterceptorChain {

	return &InterceptorChain{
		state:                     waitingToStart,
		ntfnServer:                subscribe.NewServer(),
		noMacaroons:               noMacaroons,
		permissionMap:             make(map[string][]bakery.Op),
		rpcsLog:                   log,
		registeredMiddlewareNames: make(map[string]int),
		mandatoryMiddleware:       mandatoryMiddleware,
		quit:                      make(chan struct{}),
	}
}

// Start starts the InterceptorChain, which is needed to start the state
// subscription server it powers.
func (r *InterceptorChain) Start() error {
	var err error
	r.started.Do(func() {
		err = r.ntfnServer.Start()
	})

	return err
}

// Stop stops the InterceptorChain and its internal state subscription server.
func (r *InterceptorChain) Stop() error {
	var err error
	r.stopped.Do(func() {
		close(r.quit)
		err = r.ntfnServer.Stop()
	})

	return err
}

// SetWalletNotCreated moves the RPC state from either waitingToStart to
// walletNotCreated.
func (r *InterceptorChain) SetWalletNotCreated() {
	r.Lock()
	defer r.Unlock()

	r.state = walletNotCreated
	_ = r.ntfnServer.SendUpdate(r.state)
}

// SetWalletLocked moves the RPC state from either walletNotCreated to
// walletLocked.
func (r *InterceptorChain) SetWalletLocked() {
	r.Lock()
	defer r.Unlock()

	r.state = walletLocked
	_ = r.ntfnServer.SendUpdate(r.state)
}

// SetWalletUnlocked moves the RPC state from either walletNotCreated or
// walletLocked to walletUnlocked.
func (r *InterceptorChain) SetWalletUnlocked() {
	r.Lock()
	defer r.Unlock()

	r.state = walletUnlocked
	_ = r.ntfnServer.SendUpdate(r.state)
}

// SetRPCActive moves the RPC state from walletUnlocked to rpcActive.
func (r *InterceptorChain) SetRPCActive() {
	r.Lock()
	defer r.Unlock()

	r.state = rpcActive
	_ = r.ntfnServer.SendUpdate(r.state)
}

// SetServerActive moves the RPC state from walletUnlocked to rpcActive.
func (r *InterceptorChain) SetServerActive() {
	r.Lock()
	defer r.Unlock()

	r.state = serverActive
	_ = r.ntfnServer.SendUpdate(r.state)
}

// rpcStateToWalletState converts rpcState to lnrpc.WalletState. Returns
// WAITING_TO_START and an error on conversion error.
func rpcStateToWalletState(state rpcState) (lnrpc.WalletState, error) {
	const defaultState = lnrpc.WalletState_WAITING_TO_START
	var walletState lnrpc.WalletState

	switch state {
	case waitingToStart:
		walletState = lnrpc.WalletState_WAITING_TO_START
	case walletNotCreated:
		walletState = lnrpc.WalletState_NON_EXISTING
	case walletLocked:
		walletState = lnrpc.WalletState_LOCKED
	case walletUnlocked:
		walletState = lnrpc.WalletState_UNLOCKED
	case rpcActive:
		walletState = lnrpc.WalletState_RPC_ACTIVE
	case serverActive:
		walletState = lnrpc.WalletState_SERVER_ACTIVE

	default:
		return defaultState, fmt.Errorf("unknown wallet state %v", state)
	}

	return walletState, nil
}

// SubscribeState subscribes to the state of the wallet. The current wallet
// state will always be delivered immediately.
//
// NOTE: Part of the StateService interface.
func (r *InterceptorChain) SubscribeState(_ *lnrpc.SubscribeStateRequest,
	stream lnrpc.State_SubscribeStateServer) error {

	sendStateUpdate := func(state rpcState) error {
		walletState, err := rpcStateToWalletState(state)
		if err != nil {
			return err
		}

		return stream.Send(&lnrpc.SubscribeStateResponse{
			State: walletState,
		})
	}

	// Subscribe to state updates.
	client, err := r.ntfnServer.Subscribe()
	if err != nil {
		return err
	}
	defer client.Cancel()

	// Always start by sending the current state.
	r.RLock()
	state := r.state
	r.RUnlock()

	if err := sendStateUpdate(state); err != nil {
		return err
	}

	for {
		select {
		case e := <-client.Updates():
			newState := e.(rpcState)

			// Ignore already sent state.
			if newState == state {
				continue
			}

			state = newState
			err := sendStateUpdate(state)
			if err != nil {
				return err
			}

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-stream.Context().Done():
			if errors.Is(stream.Context().Err(), context.Canceled) {
				return nil
			}
			return stream.Context().Err()

		case <-r.quit:
			return fmt.Errorf("server exiting")
		}
	}
}

// GetState returns the current wallet state.
func (r *InterceptorChain) GetState(_ context.Context,
	_ *lnrpc.GetStateRequest) (*lnrpc.GetStateResponse, error) {

	r.RLock()
	state := r.state
	r.RUnlock()

	walletState, err := rpcStateToWalletState(state)
	if err != nil {
		return nil, err
	}

	return &lnrpc.GetStateResponse{
		State: walletState,
	}, nil
}

// AddMacaroonService adds a macaroon service to the interceptor. After this is
// done every RPC call made will have to pass a valid macaroon to be accepted.
func (r *InterceptorChain) AddMacaroonService(svc *macaroons.Service) {
	r.Lock()
	defer r.Unlock()

	r.svc = svc
}

// MacaroonService returns the currently registered macaroon service. This might
// be nil if none was registered (yet).
func (r *InterceptorChain) MacaroonService() *macaroons.Service {
	r.RLock()
	defer r.RUnlock()

	return r.svc
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

// RegisterMiddleware registers a new middleware that will handle request/
// response interception for all RPC messages that are initiated with a custom
// macaroon caveat. The name of the custom caveat a middleware is handling is
// also its unique identifier. Only one middleware can be registered for each
// custom caveat.
func (r *InterceptorChain) RegisterMiddleware(mw *MiddlewareHandler) error {
	r.Lock()
	defer r.Unlock()

	// The name of the middleware is the unique identifier.
	_, ok := r.registeredMiddlewareNames[mw.middlewareName]
	if ok {
		return fmt.Errorf("a middleware with the name '%s' is already "+
			"registered", mw.middlewareName)
	}

	// For now, we only want one middleware per custom caveat name. If we
	// allowed multiple middlewares handling the same caveat there would be
	// a need for extra call chaining logic, and they could overwrite each
	// other's responses.
	for _, middleware := range r.registeredMiddleware {
		if middleware.customCaveatName == mw.customCaveatName {
			return fmt.Errorf("a middleware is already registered "+
				"for the custom caveat name '%s': %v",
				mw.customCaveatName, middleware.middlewareName)
		}
	}

	r.registeredMiddleware = append(r.registeredMiddleware, mw)
	index := len(r.registeredMiddleware) - 1
	r.registeredMiddlewareNames[mw.middlewareName] = index

	return nil
}

// RemoveMiddleware removes the middleware that handles the given custom caveat
// name.
func (r *InterceptorChain) RemoveMiddleware(middlewareName string) {
	r.Lock()
	defer r.Unlock()

	log.Debugf("Removing middleware %s", middlewareName)

	index, ok := r.registeredMiddlewareNames[middlewareName]
	if !ok {
		return
	}
	delete(r.registeredMiddlewareNames, middlewareName)

	r.registeredMiddleware = append(
		r.registeredMiddleware[:index],
		r.registeredMiddleware[index+1:]...,
	)

	// Re-initialise the middleware look-up map with the updated indexes.
	r.registeredMiddlewareNames = make(map[string]int)
	for i, mw := range r.registeredMiddleware {
		r.registeredMiddlewareNames[mw.middlewareName] = i
	}
}

// CustomCaveatSupported makes sure a middleware that handles the given custom
// caveat name is registered. If none is, an error is returned, signalling to
// the macaroon bakery and its validator to reject macaroons that have a custom
// caveat with that name.
//
// NOTE: This method is part of the macaroons.CustomCaveatAcceptor interface.
func (r *InterceptorChain) CustomCaveatSupported(customCaveatName string) error {
	r.RLock()
	defer r.RUnlock()

	// We only accept requests with a custom caveat if we also have a
	// middleware registered that handles that custom caveat. That is
	// crucial for security! Otherwise a request with an encumbered (=has
	// restricted permissions based upon the custom caveat condition)
	// macaroon would not be validated against the limitations that the
	// custom caveat implicate. Since the map is keyed by the _name_ of the
	// middleware, we need to loop through all of them to see if one has
	// the given custom macaroon caveat name.
	for _, middleware := range r.registeredMiddleware {
		if middleware.customCaveatName == customCaveatName {
			return nil
		}
	}

	return fmt.Errorf("cannot accept macaroon with custom caveat '%s', "+
		"no middleware registered to handle it", customCaveatName)
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

	// Next, we'll add the interceptors for our custom macaroon caveat based
	// middleware.
	unaryInterceptors = append(
		unaryInterceptors, r.middlewareUnaryServerInterceptor(),
	)
	strmInterceptors = append(
		strmInterceptors, r.middlewareStreamServerInterceptor(),
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
	// The StateService is being accessed, we allow the call regardless of
	// the current state.
	_, ok := srv.(lnrpc.StateServer)
	if ok {
		return nil
	}

	r.RLock()
	state := r.state
	r.RUnlock()

	switch state {
	// Do not accept any RPC calls (unless to the state service) until LND
	// has not started.
	case waitingToStart:
		return ErrWaitingToStart

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

	// If the RPC server or lnd server is active, we allow calls to any
	// service except the WalletUnlocker.
	case rpcActive, serverActive:
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

		r.rpcsLog.Debugf("[%v] requested", info.FullMethod)

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

		r.rpcsLog.Debugf("[%v] requested", info.FullMethod)

		if err := r.checkRPCState(srv); err != nil {
			return err
		}

		return handler(srv, ss)
	}
}

// middlewareUnaryServerInterceptor is a unary gRPC interceptor that intercepts
// all requests and responses that are sent with a macaroon containing a custom
// caveat condition that is handled by registered middleware.
func (r *InterceptorChain) middlewareUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context,
		req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		// Make sure we don't allow any requests through if one of the
		// mandatory middlewares is missing.
		fullMethod := info.FullMethod
		if err := r.checkMandatoryMiddleware(fullMethod); err != nil {
			return nil, err
		}

		// If there is no middleware registered, we don't need to
		// intercept anything.
		if !r.middlewareRegistered() {
			return handler(ctx, req)
		}

		requestID := atomic.AddUint64(&r.lastRequestID, 1)
		req, err := r.interceptMessage(
			ctx, TypeRequest, requestID, false, info.FullMethod,
			req,
		)
		if err != nil {
			return nil, err
		}

		// Call the handler, which executes the request against lnd.
		lndResp, lndErr := handler(ctx, req)
		if lndErr != nil {
			// The call to lnd ended in an error and not a normal
			// proto message response. Send the error to the
			// interceptor as well to inform about the abnormal
			// termination of the stream and to give the option to
			// replace the error message with a custom one.
			replacedErr, err := r.interceptMessage(
				ctx, TypeResponse, requestID, false,
				info.FullMethod, lndErr,
			)
			if err != nil {
				return nil, err
			}
			return lndResp, replacedErr.(error)
		}

		return r.interceptMessage(
			ctx, TypeResponse, requestID, false, info.FullMethod,
			lndResp,
		)
	}
}

// middlewareStreamServerInterceptor is a streaming gRPC interceptor that
// intercepts all requests and responses that are sent with a macaroon
// containing a custom caveat condition that is handled by registered
// middleware.
func (r *InterceptorChain) middlewareStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{},
		ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) error {

		// Don't intercept the interceptor itself which is a streaming
		// RPC too!
		fullMethod := info.FullMethod
		if fullMethod == lnrpc.RegisterRPCMiddlewareURI {
			return handler(srv, ss)
		}

		// Make sure we don't allow any requests through if one of the
		// mandatory middlewares is missing. We add this check here to
		// make sure the middleware registration itself can still be
		// called.
		if err := r.checkMandatoryMiddleware(fullMethod); err != nil {
			return err
		}

		// If there is no middleware registered, we don't need to
		// intercept anything.
		if !r.middlewareRegistered() {
			return handler(srv, ss)
		}

		// To give the middleware a chance to accept or reject the
		// establishment of the stream itself (and not only when the
		// first message is sent on the stream), we send an intercept
		// request for the stream auth now:
		msg, err := NewStreamAuthInterceptionRequest(
			ss.Context(), info.FullMethod,
		)
		if err != nil {
			return err
		}

		requestID := atomic.AddUint64(&r.lastRequestID, 1)
		err = r.acceptStream(requestID, msg)
		if err != nil {
			return err
		}

		wrappedSS := &serverStreamWrapper{
			ServerStream: ss,
			requestID:    requestID,
			fullMethod:   info.FullMethod,
			interceptor:  r,
		}

		// Call the stream handler, which will block as long as the
		// stream is alive.
		lndErr := handler(srv, wrappedSS)
		if lndErr != nil {
			// This is an error being returned from lnd. Send it to
			// the interceptor as well to inform about the abnormal
			// termination of the stream and to give the option to
			// replace the error message with a custom one.
			replacedErr, err := r.interceptMessage(
				ss.Context(), TypeResponse, requestID,
				true, info.FullMethod, lndErr,
			)
			if err != nil {
				return err
			}

			return replacedErr.(error)
		}

		// Normal/successful termination of the stream.
		return nil
	}
}

// checkMandatoryMiddleware makes sure that each of the middlewares declared as
// mandatory is currently registered.
func (r *InterceptorChain) checkMandatoryMiddleware(fullMethod string) error {
	r.RLock()
	defer r.RUnlock()

	// Allow calls that are whitelisted for macaroons as well, otherwise we
	// get into impossible situations where the wallet is locked but the
	// unlock call is denied because the middleware isn't registered. But
	// the middleware cannot register itself because the wallet is locked.
	if _, ok := macaroonWhitelist[fullMethod]; ok {
		return nil
	}

	// Not a white listed call so make sure every mandatory middleware is
	// currently connected to lnd.
	for _, name := range r.mandatoryMiddleware {
		if _, ok := r.registeredMiddlewareNames[name]; !ok {
			return fmt.Errorf("mandatory middleware '%s' is "+
				"currently not registered, not allowing any "+
				"RPC calls", name)
		}
	}

	return nil
}

// middlewareRegistered returns true if there is at least one middleware
// currently registered.
func (r *InterceptorChain) middlewareRegistered() bool {
	r.RLock()
	defer r.RUnlock()

	return len(r.registeredMiddleware) > 0
}

// acceptStream sends an intercept request to all middlewares that have
// registered for it. This means either a middleware has requested read-only
// access or the request actually has a macaroon with a caveat the middleware
// registered for.
func (r *InterceptorChain) acceptStream(requestID uint64,
	msg *InterceptionRequest) error {

	r.RLock()
	defer r.RUnlock()

	for _, middleware := range r.registeredMiddleware {
		// If there is a custom caveat in the macaroon, make sure the
		// middleware registered for it. Or if a middleware registered
		// for read-only mode, it also gets the request.
		hasCustomCaveat := macaroons.HasCustomCaveat(
			msg.Macaroon, middleware.customCaveatName,
		)
		if !hasCustomCaveat && !middleware.readOnly {
			continue
		}

		msg.CustomCaveatCondition = macaroons.GetCustomCaveatCondition(
			msg.Macaroon, middleware.customCaveatName,
		)

		resp, err := middleware.intercept(requestID, msg)

		// Error during interception itself.
		if err != nil {
			return err
		}

		// Error returned from middleware client.
		if resp.err != nil {
			return resp.err
		}
	}

	return nil
}

// interceptMessage sends out an intercept request for an RPC response. Since
// middleware that hasn't registered for the read-only mode has the option to
// overwrite/replace the message, this needs to be handled differently than the
// auth path above.
func (r *InterceptorChain) interceptMessage(ctx context.Context,
	interceptType InterceptType, requestID uint64, isStream bool,
	fullMethod string, m interface{}) (interface{}, error) {

	r.RLock()
	defer r.RUnlock()

	currentMessage := m
	for _, middleware := range r.registeredMiddleware {
		msg, err := NewMessageInterceptionRequest(
			ctx, interceptType, isStream, fullMethod,
			currentMessage,
		)
		if err != nil {
			return nil, err
		}

		// If there is a custom caveat in the macaroon, make sure the
		// middleware registered for it. Or if a middleware registered
		// for read-only mode, it also gets the request.
		hasCustomCaveat := macaroons.HasCustomCaveat(
			msg.Macaroon, middleware.customCaveatName,
		)
		if !hasCustomCaveat && !middleware.readOnly {
			continue
		}

		msg.CustomCaveatCondition = macaroons.GetCustomCaveatCondition(
			msg.Macaroon, middleware.customCaveatName,
		)

		resp, err := middleware.intercept(requestID, msg)

		// Error during interception itself.
		if err != nil {
			return nil, err
		}

		// Error returned from middleware client.
		if resp.err != nil {
			return nil, resp.err
		}

		// The message was replaced, make sure the next middleware in
		// line receives the updated message.
		if !middleware.readOnly && resp.replace {
			currentMessage = resp.replacement
		}
	}

	return currentMessage, nil
}

// serverStreamWrapper is a struct that wraps a server stream in a way that all
// requests and responses can be intercepted individually.
type serverStreamWrapper struct {
	// ServerStream is the stream that's being wrapped.
	grpc.ServerStream

	requestID uint64

	fullMethod string

	interceptor *InterceptorChain
}

// SendMsg is called when lnd sends a message to the client. This is wrapped to
// intercept streaming RPC responses.
func (w *serverStreamWrapper) SendMsg(m interface{}) error {
	newMsg, err := w.interceptor.interceptMessage(
		w.ServerStream.Context(), TypeResponse, w.requestID, true,
		w.fullMethod, m,
	)
	if err != nil {
		return err
	}

	return w.ServerStream.SendMsg(newMsg)
}

// RecvMsg is called when lnd wants to receive a message from the client. This
// is wrapped to intercept streaming RPC requests.
func (w *serverStreamWrapper) RecvMsg(m interface{}) error {
	err := w.ServerStream.RecvMsg(m)
	if err != nil {
		return err
	}

	req, err := w.interceptor.interceptMessage(
		w.ServerStream.Context(), TypeRequest, w.requestID, true,
		w.fullMethod, m,
	)
	if err != nil {
		return err
	}

	return replaceProtoMsg(m, req)
}
