package lnp2p

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// PeerServiceKey is a type alias for ServiceKey used with peer messages.
type PeerServiceKey = actor.ServiceKey[PeerMessage, PeerResponse]

// PeerBehavior is a type alias for ActorBehavior used with peer messages.
type PeerBehavior = actor.ActorBehavior[PeerMessage, PeerResponse]

// ActorWithConnConfig combines SimplePeerConfig with actor-specific configuration.
type ActorWithConnConfig struct {
	// SimplePeerCfg is the configuration for the underlying SimplePeer connection.
	SimplePeerCfg SimplePeerConfig

	// ActorSystem is the actor system to register with.
	ActorSystem *actor.ActorSystem

	// ServiceKey is the service key to register the actor under.
	ServiceKey PeerServiceKey

	// ActorName is the name to register the actor under.
	ActorName string

	// MessageSinks are the message destinations with optional filters.
	MessageSinks []*MessageSink

	// AutoConnect determines if the actor should connect on start.
	AutoConnect bool
}

// NewActorWithConn creates a new PeerActor with an underlying SimplePeer connection,
// registers it with the actor system, and sets everything up automatically.
func NewActorWithConn(cfg ActorWithConnConfig) (*PeerActor, actor.ActorRef[PeerMessage, PeerResponse], error) {
	// Create the SimplePeer connection.
	simplePeer, err := NewSimplePeer(cfg.SimplePeerCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create SimplePeer: %w", err)
	}

	// Create the PeerActorConfig with the SimplePeer as the connection.
	peerActorCfg := PeerActorConfig{
		Connection:   simplePeer,
		Receptionist: cfg.ActorSystem.Receptionist(),
		MessageSinks: cfg.MessageSinks,
		AutoConnect:  cfg.AutoConnect,
	}

	// Create the PeerActor.
	peerActor, err := NewPeerActor(peerActorCfg)
	if err != nil {
		// Clean up the SimplePeer if PeerActor creation fails.
		simplePeer.Close()
		return nil, nil, fmt.Errorf("failed to create PeerActor: %w", err)
	}

	// Register the actor with the system.
	actorRef := actor.RegisterWithSystem(
		cfg.ActorSystem,
		cfg.ActorName,
		cfg.ServiceKey,
		peerActor,
	)

	// Set the actorRef on the PeerActor so convenience methods work.
	peerActor.setActorRef(actorRef)

	return peerActor, actorRef, nil
}

// PeerActor implements an actor-based peer that distributes messages via
// ServiceKeys.
type PeerActor struct {
	cfg PeerActorConfig

	// conn is the underlying P2P connection.
	conn P2PConnection

	// messageSinks are the destinations for incoming messages.
	// No mutex needed since actor runtime is single-threaded.
	messageSinks []*MessageSink

	// sinksByKey maps service key string representations to their sinks.
	// This allows checking for duplicates without fmt.Sprintf on every
	// operation.
	sinksByKey map[string][]*MessageSink

	// connPromise tracks the connection establishment.
	connPromise actor.Promise[ConnectionResult]

	// peerLog is the logger for this peer actor with proper prefix.
	peerLog btclog.Logger

	// wg tracks all goroutines started by this actor.
	wg sync.WaitGroup

	// quit is closed to signal all goroutines to exit.
	quit chan struct{}

	// stopOnce ensures Stop is only executed once.
	stopOnce sync.Once

	// actorRef is the actor's own reference, set after registration.
	actorRef actor.ActorRef[PeerMessage, PeerResponse]

	isRunning     atomic.Bool
	lastError     error
	messageCount  atomic.Uint64
	connectedTime time.Time
}

// MessageFilter is a predicate function that determines if a message should be
// processed.
type MessageFilter func(lnwire.Message) bool

// MessageSink represents a destination for peer messages with optional
// filtering.
type MessageSink struct {
	// ServiceKey is the actor service key to route messages to.
	ServiceKey PeerServiceKey

	// Filter is an optional message filter. If nil, all messages are
	// accepted.
	Filter MessageFilter

	// actorRef is the resolved actor reference (cached).
	actorRef actor.ActorRef[PeerMessage, PeerResponse]
}

// ShouldReceive returns true if this sink should receive the given message.
func (m *MessageSink) ShouldReceive(msg lnwire.Message) bool {
	if m.Filter == nil {
		return true
	}
	return m.Filter(msg)
}

// PeerActorConfig configures a PeerActor.
type PeerActorConfig struct {
	// Connection is the P2P connection to use.
	Connection P2PConnection

	// MessageSinks are the message destinations with optional filters.
	MessageSinks []*MessageSink

	// Receptionist is used to resolve ServiceKeys to ActorRefs.
	Receptionist *actor.Receptionist

	// AutoConnect determines if the actor should connect on start.
	AutoConnect bool
}

// ConnectionResult represents the result of a connection attempt.
type ConnectionResult struct {
	// Success indicates if the connection was successful.
	Success bool

	// RemotePubKey is the remote peer's public key if connected.
	RemotePubKey *btcec.PublicKey

	// ConnectedAt is when the connection was established.
	ConnectedAt time.Time

	// Error contains any connection error.
	Error error
}

// NewPeerActor creates a new PeerActor instance.
func NewPeerActor(cfg PeerActorConfig) (*PeerActor, error) {
	// Validate configuration.
	if err := ValidatePeerActorConfig(cfg); err != nil {
		return nil, err
	}

	// Create the peer-specific logger with appropriate prefix.
	var peerPrefix string
	if cfg.Connection != nil {
		// Try to get remote pubkey from connection if available.
		if remotePub := cfg.Connection.RemotePubKey(); remotePub != nil {
			if remoteAddr := cfg.Connection.RemoteAddr(); remoteAddr != nil {
				peerPrefix = fmt.Sprintf("PeerActor(%x@%s):",
					remotePub.SerializeCompressed(),
					remoteAddr)
			} else {
				peerPrefix = fmt.Sprintf("PeerActor(%x):",
					remotePub.SerializeCompressed())
			}
		} else {
			peerPrefix = "PeerActor(<unknown>):"
		}
	} else {
		peerPrefix = "PeerActor(<unconnected>):"
	}
	peerLog := log.WithPrefix(peerPrefix)

	p := &PeerActor{
		cfg:          cfg,
		conn:         cfg.Connection,
		messageSinks: make([]*MessageSink, 0, len(cfg.MessageSinks)),
		sinksByKey:   make(map[string][]*MessageSink),
		connPromise:  actor.NewPromise[ConnectionResult](),
		peerLog:      peerLog,
		quit:         make(chan struct{}),
	}

	for _, sink := range cfg.MessageSinks {
		p.addMessageSinkInternal(sink)
	}

	return p, nil
}

// addMessageSinkInternal adds a message sink without locking (caller must hold
// lock).
func (p *PeerActor) addMessageSinkInternal(sink *MessageSink) {
	keyStr := fmt.Sprintf("%v", sink.ServiceKey)

	p.messageSinks = append(p.messageSinks, sink)
	p.sinksByKey[keyStr] = append(p.sinksByKey[keyStr], sink)

	// Resolve and cache actor refs in the sink.
	refs := actor.FindInReceptionist(p.cfg.Receptionist, sink.ServiceKey)
	if len(refs) > 0 {
		// We'll just use the first ref for now. In the future, we can
		// wrap/use a router.
		sink.actorRef = refs[0]
	} else {
		// Log warning but continue - actors might register later.
		p.peerLog.Warnf("No actors found for service key %v",
			sink.ServiceKey)
	}
}

// Receive implements actor.ActorBehavior.
func (p *PeerActor) Receive(ctx context.Context, msg PeerMessage) fn.Result[PeerResponse] {
	switch m := msg.(type) {
	case *ConnectRequest:
		return p.handleConnect(ctx, m)

	case *DisconnectRequest:
		return p.handleDisconnect(ctx, m)

	case *SendMessageRequest:
		return p.handleSendMessage(ctx, m)

	case *GetStatusRequest:
		return p.handleGetStatus(ctx, m)

	case *WaitForConnectionRequest:
		return p.handleWaitForConnection(ctx, m)

	case *AddServiceKeyRequest:
		return p.handleAddServiceKey(ctx, m)

	case *RemoveServiceKeyRequest:
		return p.handleRemoveServiceKey(ctx, m)

	case *GetServiceKeysRequest:
		return p.handleGetServiceKeys(ctx, m)

	case *GetMessageSinksRequest:
		return p.handleGetMessageSinks(ctx, m)

	default:
		return fn.Err[PeerResponse](fmt.Errorf("unknown "+
			"message type: %T", msg))
	}
}

// handleConnect processes connection requests.
func (p *PeerActor) handleConnect(ctx context.Context,
	req *ConnectRequest) fn.Result[PeerResponse] {

	// If we're already connected, then return immediately. We'll also make
	// sure that the message processor is running.
	if p.conn.IsConnected() {
		// Start message processor if not already running.
		if p.isRunning.CompareAndSwap(false, true) {
			p.wg.Add(1)
			go p.processMessages(ctx)
		}

		return fn.Ok[PeerResponse](&ConnectResponse{
			Success:          true,
			RemotePubKey:     p.conn.RemotePubKey(),
			AlreadyConnected: true,
		})
	}

	// We'll use a goroutine to attempt to connect in the background, so we
	// can return a future immediately to the caller.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.peerLog.Infof("Attempting to connect to remote peer")

		err := p.conn.Connect(ctx)

		result := ConnectionResult{
			Success:     err == nil,
			Error:       err,
			ConnectedAt: time.Now(),
		}

		if err == nil {
			result.RemotePubKey = p.conn.RemotePubKey()
			p.connectedTime = result.ConnectedAt

			p.peerLog.Infof("Successfully connected to remote peer")

			// Start message processor if not already running.
			if p.isRunning.CompareAndSwap(false, true) {
				p.peerLog.Debugf("Starting message processor")

				p.wg.Add(1)
				go p.processMessages(ctx)
			}
		} else {
			p.peerLog.Errorf("Failed to connect: %v", err)
		}

		p.connPromise.Complete(fn.NewResult(result, err))
	}()

	return fn.Ok[PeerResponse](&ConnectResponse{
		Success: true,
		Future:  p.connPromise.Future(),
	})
}

// handleDisconnect processes disconnection requests.
func (p *PeerActor) handleDisconnect(ctx context.Context,
	req *DisconnectRequest) fn.Result[PeerResponse] {

	err := p.conn.Close()

	return fn.NewResult[PeerResponse](&DisconnectResponse{
		Success: err == nil,
		Error:   err,
	}, err)
}

// handleSendMessage processes message send requests.
func (p *PeerActor) handleSendMessage(ctx context.Context,
	req *SendMessageRequest) fn.Result[PeerResponse] {

	if !p.conn.IsConnected() {
		return fn.Err[PeerResponse](fmt.Errorf("not connected"))
	}

	err := p.conn.SendMessage(req.Message)
	if err != nil {
		return fn.Err[PeerResponse](fmt.Errorf("failed to "+
			"send message: %w", err))
	}

	return fn.Ok[PeerResponse](&SendMessageResponse{
		Success: true,
	})
}

// handleGetStatus processes status requests.
func (p *PeerActor) handleGetStatus(ctx context.Context,
	req *GetStatusRequest) fn.Result[PeerResponse] {

	status := &StatusResponse{
		IsConnected:   p.conn.IsConnected(),
		RemotePubKey:  p.conn.RemotePubKey(),
		LocalPubKey:   p.conn.LocalPubKey(),
		MessageCount:  p.messageCount.Load(),
		ConnectedTime: p.connectedTime,
		LastError:     p.lastError,
	}

	if p.conn.IsConnected() {
		status.RemoteAddr = p.conn.RemoteAddr().String()
		status.LocalAddr = p.conn.LocalAddr().String()
	}

	return fn.Ok[PeerResponse](status)
}

// handleWaitForConnection returns a future that completes when connected.
func (p *PeerActor) handleWaitForConnection(ctx context.Context,
	req *WaitForConnectionRequest) fn.Result[PeerResponse] {

	// If already connected, return immediately.
	if p.conn.IsConnected() {
		return fn.Ok[PeerResponse](&WaitForConnectionResponse{
			Future:           p.connPromise.Future(),
			AlreadyConnected: true,
		})
	}

	return fn.Ok[PeerResponse](&WaitForConnectionResponse{
		Future: p.connPromise.Future(),
	})
}

// processMessages reads and distributes incoming messages to ServiceKeys.
func (p *PeerActor) processMessages(ctx context.Context) {
	// isRunning is already set by the caller using CompareAndSwap.
	defer func() {
		p.wg.Done()
		p.isRunning.Store(false)

		p.peerLog.Debugf("Message processor stopped")
	}()

	p.peerLog.Debugf("Message processor started")

	// We'll process messages using the iterator of the underlying peer
	// connection.
	for msg := range p.conn.ReceiveMessages() {
		// Check if we should quit.
		select {
		case <-p.quit:
			return
		case <-ctx.Done():
			return
		default:
		}

		p.messageCount.Add(1)

		// Handle ping/pong internally.
		switch m := msg.(type) {
		case *lnwire.Ping:
			p.peerLog.Tracef("Received ping, sending pong")

			pongData := make([]byte, m.NumPongBytes)
			pong := lnwire.NewPong(pongData)
			p.conn.SendMessage(pong)
			continue

		case *lnwire.Pong:
			p.peerLog.Tracef("Received pong")

			continue
		}

		// Create a new message received event, then distributed it to
		// all registered message sinks.
		msgReceived := &MessageReceived{
			Message:    msg,
			From:       p.conn.RemotePubKey(),
			ReceivedAt: time.Now(),
		}

		p.distributeMessage(ctx, msgReceived)

		// We'll also handle some of the special messages internally,
		// such as warning+error.
		switch m := msg.(type) {
		case *lnwire.Error:
			p.peerLog.Errorf("Received error message: %s", m.Data)

			peerErr := &PeerError{
				Error: fmt.Errorf(
					"remote error: %s", m.Data,
				),
				ErrorType: ErrorTypeRemote,
				From:      p.conn.RemotePubKey(),
				Timestamp: time.Now(),
			}
			p.distributeMessage(ctx, peerErr)

		case *lnwire.Warning:
			p.peerLog.Warnf("Received warning message: %s", m.Data)

			peerWarn := &PeerWarning{
				Warning:   string(m.Data),
				From:      p.conn.RemotePubKey(),
				Timestamp: time.Now(),
			}
			p.distributeMessage(ctx, peerWarn)
		}

		// Check if context is cancelled.
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// distributeMessage sends a message to all registered actors using Tell.
func (p *PeerActor) distributeMessage(ctx context.Context, msg PeerMessage) {
	var lnwireMsg lnwire.Message
	if msgRcvd, ok := msg.(*MessageReceived); ok {
		lnwireMsg = msgRcvd.Message
	}

	for _, sink := range p.messageSinks {
		if lnwireMsg != nil && !sink.ShouldReceive(lnwireMsg) {
			continue
		}

		if sink.actorRef == nil {
			refs := actor.FindInReceptionist(
				p.cfg.Receptionist, sink.ServiceKey,
			)
			if len(refs) > 0 {
				sink.actorRef = refs[0]
			}
		}

		if sink.actorRef != nil {
			sink.actorRef.Tell(ctx, msg)
		}
	}
}

// refreshActorRefs updates the actor references for all sinks.
func (p *PeerActor) refreshActorRefs() {
	for _, sink := range p.messageSinks {
		refs := actor.FindInReceptionist(
			p.cfg.Receptionist, sink.ServiceKey,
		)
		if len(refs) > 0 {
			sink.actorRef = refs[0]
		} else {
			sink.actorRef = nil
		}
	}
}

// handleAddServiceKey processes requests to add a new service key.
func (p *PeerActor) handleAddServiceKey(ctx context.Context,
	req *AddServiceKeyRequest) fn.Result[PeerResponse] {

	if req.MessageSink == nil {
		p.peerLog.Warnf("AddServiceKeyRequest missing MessageSink")
		return fn.Ok[PeerResponse](&AddServiceKeyResponse{
			Success: false,
		})
	}

	keyStr := fmt.Sprintf("%v", req.MessageSink.ServiceKey)

	// Check if a sink with this key already exists.
	if _, exists := p.sinksByKey[keyStr]; exists {
		p.peerLog.Debugf("Service key already exists: %v", keyStr)
		return fn.Ok[PeerResponse](&AddServiceKeyResponse{
			Success:         false,
			AlreadyExists:   true,
			CurrentKeyCount: len(p.messageSinks),
		})
	}

	p.peerLog.Debugf("Adding service key %v with filter: %v",
		keyStr, req.MessageSink.Filter != nil)

	// Use the MessageSink from the request directly.
	p.addMessageSinkInternal(req.MessageSink)

	return fn.Ok[PeerResponse](&AddServiceKeyResponse{
		Success:         true,
		CurrentKeyCount: len(p.messageSinks),
	})
}

// handleRemoveServiceKey processes requests to remove a service key.
func (p *PeerActor) handleRemoveServiceKey(ctx context.Context,
	req *RemoveServiceKeyRequest) fn.Result[PeerResponse] {

	keyStr := fmt.Sprintf("%v", req.ServiceKey)

	sinks, exists := p.sinksByKey[keyStr]
	if !exists || len(sinks) == 0 {
		return fn.Ok[PeerResponse](&RemoveServiceKeyResponse{
			Success:         false,
			NotFound:        true,
			CurrentKeyCount: len(p.sinksByKey),
		})
	}

	delete(p.sinksByKey, keyStr)

	// Rebuild messageSinks slice without the removed sinks.
	newSinks := make([]*MessageSink, 0, len(p.messageSinks)-len(sinks))
	for _, sink := range p.messageSinks {
		if fmt.Sprintf("%v", sink.ServiceKey) != keyStr {
			newSinks = append(newSinks, sink)
		}
	}
	p.messageSinks = newSinks

	return fn.Ok[PeerResponse](&RemoveServiceKeyResponse{
		Success:         true,
		CurrentKeyCount: len(p.sinksByKey),
	})
}

// handleGetServiceKeys returns the current list of service keys.
func (p *PeerActor) handleGetServiceKeys(ctx context.Context,
	req *GetServiceKeysRequest) fn.Result[PeerResponse] {

	// Extract unique service keys from sinks.
	keyMap := make(map[string]PeerServiceKey)
	for _, sink := range p.messageSinks {
		keyStr := fmt.Sprintf("%v", sink.ServiceKey)
		keyMap[keyStr] = sink.ServiceKey
	}

	// Use maps.Values to get all unique keys and collect into slice.
	keys := slices.Collect(maps.Values(keyMap))

	// Count actors with refs.
	actorCount := 0
	for _, sink := range p.messageSinks {
		if sink.actorRef != nil {
			actorCount++
		}
	}

	return fn.Ok[PeerResponse](&GetServiceKeysResponse{
		ServiceKeys: keys,
		ActorCount:  actorCount,
	})
}

// handleGetMessageSinks returns the current list of message sinks.
func (p *PeerActor) handleGetMessageSinks(ctx context.Context,
	req *GetMessageSinksRequest) fn.Result[PeerResponse] {

	// Make a defensive copy to avoid external mutations.
	sinks := make([]*MessageSink, len(p.messageSinks))
	copy(sinks, p.messageSinks)

	return fn.Ok[PeerResponse](&GetMessageSinksResponse{
		MessageSinks: sinks,
	})
}

// Start starts the peer actor.
func (p *PeerActor) Start(ctx context.Context) fn.Result[ConnectionResult] {
	if p.cfg.AutoConnect {
		result := p.handleConnect(ctx, &ConnectRequest{})
		if resp, err := result.Unpack(); err == nil {
			if connResp, ok := resp.(*ConnectResponse); ok {
				if connResp.AlreadyConnected {
					return fn.Ok(ConnectionResult{
						Success:      true,
						RemotePubKey: connResp.RemotePubKey,
						ConnectedAt:  time.Now(),
					})
				}

				if connResp.Future != nil {
					futureResult := connResp.Future.Await(
						ctx,
					)
					val, err := futureResult.Unpack()
					if err == nil {
						return fn.Ok(val)
					}
				}
			}
		}

		return fn.Ok(ConnectionResult{Success: false})
	}

	return fn.Ok(ConnectionResult{Success: true})
}

// Stop stops the peer actor and waits for all goroutines to finish.
func (p *PeerActor) Stop() error {
	var connErr error
	p.stopOnce.Do(func() {
		p.peerLog.Infof("Stopping peer actor")

		close(p.quit)
		connErr = p.conn.Close()
		p.wg.Wait()

		p.peerLog.Debugf("Peer actor stopped")
	})

	return connErr
}

// ConnectionFuture returns a future that completes when the connection is
// established.
func (p *PeerActor) ConnectionFuture() actor.Future[ConnectionResult] {
	return p.connPromise.Future()
}

// setActorRef sets the actor's own reference for use in convenience methods.
// This should be called after registering the actor with the system.
func (p *PeerActor) setActorRef(ref actor.ActorRef[PeerMessage, PeerResponse]) {
	p.actorRef = ref
}

// AddServiceKey is a convenience method that adds a service key by sending an
// AddServiceKeyRequest through the actor system.
func (p *PeerActor) AddServiceKey(key PeerServiceKey) bool {
	if p.actorRef == nil {
		return false
	}

	ctx := context.Background()
	future := p.actorRef.Ask(ctx, &AddServiceKeyRequest{
		MessageSink: &MessageSink{
			ServiceKey: key,
			Filter:     nil,
		},
	})

	result := future.Await(ctx)
	resp, err := result.Unpack()
	if err != nil {
		return false
	}

	addResp := resp.(*AddServiceKeyResponse)
	return addResp.Success
}

// AddMessageSink is a convenience method that adds a message sink by sending an
// AddServiceKeyRequest through the actor system.
func (p *PeerActor) AddMessageSink(sink *MessageSink) bool {
	if p.actorRef == nil {
		return false
	}

	ctx := context.Background()
	future := p.actorRef.Ask(ctx, &AddServiceKeyRequest{
		MessageSink: sink,
	})

	result := future.Await(ctx)
	resp, err := result.Unpack()
	if err != nil {
		return false
	}

	addResp := resp.(*AddServiceKeyResponse)
	return addResp.Success
}

// RemoveServiceKey is a convenience method that removes a service key by
// sending a RemoveServiceKeyRequest through the actor system.
func (p *PeerActor) RemoveServiceKey(key PeerServiceKey) bool {
	if p.actorRef == nil {
		return false
	}

	ctx := context.Background()
	future := p.actorRef.Ask(ctx, &RemoveServiceKeyRequest{
		ServiceKey: key,
	})

	result := future.Await(ctx)
	resp, err := result.Unpack()
	if err != nil {
		return false
	}

	removeResp := resp.(*RemoveServiceKeyResponse)
	return removeResp.Success
}

// GetServiceKeys is a convenience method that retrieves service keys by sending
// a GetServiceKeysRequest through the actor system.
func (p *PeerActor) GetServiceKeys() []PeerServiceKey {
	if p.actorRef == nil {
		return nil
	}

	ctx := context.Background()
	future := p.actorRef.Ask(ctx, &GetServiceKeysRequest{})

	result := future.Await(ctx)
	resp, err := result.Unpack()
	if err != nil {
		return nil
	}

	keysResp := resp.(*GetServiceKeysResponse)
	return keysResp.ServiceKeys
}

// GetMessageSinks is a convenience method that retrieves message sinks by
// sending a GetMessageSinksRequest through the actor system.
func (p *PeerActor) GetMessageSinks() []*MessageSink {
	if p.actorRef == nil {
		return nil
	}

	ctx := context.Background()
	future := p.actorRef.Ask(ctx, &GetMessageSinksRequest{})

	result := future.Await(ctx)
	resp, err := result.Unpack()
	if err != nil {
		return nil
	}

	sinksResp := resp.(*GetMessageSinksResponse)
	return sinksResp.MessageSinks
}

// GetStatus is a convenience method that retrieves the peer status by
// sending a GetStatusRequest through the actor system.
func (p *PeerActor) GetStatus() *StatusResponse {
	if p.actorRef == nil {
		return nil
	}

	ctx := context.Background()
	future := p.actorRef.Ask(ctx, &GetStatusRequest{})

	result := future.Await(ctx)
	resp, err := result.Unpack()
	if err != nil {
		return nil
	}

	statusResp := resp.(*StatusResponse)
	return statusResp
}

// CreatePeerService creates a service key and registers the actor with a
// system.
func CreatePeerService(system *actor.ActorSystem,
	id string, cfg PeerActorConfig,
) (actor.ActorRef[PeerMessage, PeerResponse], error) {

	if cfg.Receptionist == nil {
		cfg.Receptionist = system.Receptionist()
	}

	// Create the service key for the actor, based on the ID passed in.
	serviceKey := actor.NewServiceKey[PeerMessage, PeerResponse](id)
	peerActor, err := NewPeerActor(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create peer actor: %w", err)
	}

	// PeerActor directly implements ActorBehavior, no adapter needed.
	actorRef := actor.RegisterWithSystem(system, id, serviceKey, peerActor)

	// Set the actor's own reference for convenience methods.
	peerActor.setActorRef(actorRef)

	return actorRef, nil
}

// Ensure PeerActor implements the ActorBehavior interface directly.
var _ PeerBehavior = (*PeerActor)(nil)

// ValidatePeerActorConfig validates the PeerActor configuration.
func ValidatePeerActorConfig(cfg PeerActorConfig) error {
	if cfg.Connection == nil {
		return fmt.Errorf("connection is required")
	}

	if cfg.Receptionist == nil {
		return fmt.Errorf("receptionist is required")
	}

	return nil
}
