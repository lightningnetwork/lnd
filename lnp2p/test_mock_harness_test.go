package lnp2p

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockConnectionConfig defines the configuration for a mock connection.
type MockConnectionConfig struct {
	// IsConnected determines the connection state.
	IsConnected bool

	// RemotePubKey is the remote peer's public key.
	RemotePubKey *btcec.PublicKey

	// LocalPubKey is the local peer's public key.
	LocalPubKey *btcec.PublicKey

	// RemoteAddr is the remote address.
	RemoteAddr net.Addr

	// LocalAddr is the local address.
	LocalAddr net.Addr

	// ConnectError is the error to return on Connect.
	ConnectError error

	// CloseError is the error to return on Close.
	CloseError error

	// SendMessageError is the error to return on SendMessage.
	SendMessageError error

	// Messages to yield from ReceiveMessages.
	Messages []lnwire.Message

	// Events to yield from ConnectionEvents.
	Events []ConnectionEvent
}

// DefaultMockConfig returns a default mock configuration.
func DefaultMockConfig() *MockConnectionConfig {
	return &MockConnectionConfig{
		IsConnected:  false,
		RemotePubKey: nil,
		LocalPubKey:  nil,
		RemoteAddr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		LocalAddr:    &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345},
	}
}

// ConnectedMockConfig returns a mock configuration for a connected peer.
func ConnectedMockConfig() *MockConnectionConfig {
	return &MockConnectionConfig{
		IsConnected:  true,
		RemotePubKey: newTestPubKey(),
		LocalPubKey:  newTestPubKey(),
		RemoteAddr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		LocalAddr:    &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345},
	}
}

// testHarness provides utilities for testing with mocked connections.
type testHarness struct {
	t       *testing.T
	mock    *mockP2PConnection
	cfg     *MockConnectionConfig
	system  *actor.ActorSystem
	cleanup []func()
}

// newTestHarness creates a new test harness with the given configuration.
func newTestHarness(t *testing.T, cfg *MockConnectionConfig) *testHarness {
	if cfg == nil {
		cfg = DefaultMockConfig()
	}

	h := &testHarness{
		t:       t,
		mock:    &mockP2PConnection{},
		cfg:     cfg,
		cleanup: []func(){},
	}

	h.setupBasicExpectations()
	return h
}

// setupBasicExpectations sets up the basic mock expectations.
func (h *testHarness) setupBasicExpectations() {
	// Always set up basic getters.
	h.mock.On("IsConnected").Return(h.cfg.IsConnected).Maybe()
	h.mock.On("RemotePubKey").Return(h.cfg.RemotePubKey).Maybe()
	h.mock.On("LocalPubKey").Return(h.cfg.LocalPubKey).Maybe()

	// Always expect Close to be called.
	h.mock.On("Close").Return(nil).Maybe()

	// Always set up address getters (may return nil when not connected).
	h.mock.On("RemoteAddr").Return(h.cfg.RemoteAddr).Maybe()
	h.mock.On("LocalAddr").Return(h.cfg.LocalAddr).Maybe()

	// Set up iterators with empty defaults if not specified.
	h.setupMessageIterator()
	h.setupEventIterator()
}

// setupMessageIterator sets up the ReceiveMessages mock.
func (h *testHarness) setupMessageIterator() {
	if len(h.cfg.Messages) > 0 {
		h.mock.On("ReceiveMessages").Return(func(yield func(lnwire.Message) bool) {
			for _, msg := range h.cfg.Messages {
				if !yield(msg) {
					break
				}
			}
		}).Maybe()
	} else {
		// Return empty iterator.
		h.mock.On("ReceiveMessages").Return(func(yield func(lnwire.Message) bool) {}).Maybe()
	}
}

// setupEventIterator sets up the ConnectionEvents mock.
func (h *testHarness) setupEventIterator() {
	if len(h.cfg.Events) > 0 {
		h.mock.On("ConnectionEvents").Return(func(yield func(ConnectionEvent) bool) {
			for _, event := range h.cfg.Events {
				if !yield(event) {
					break
				}
			}
		}).Maybe()
	} else {
		// Return empty iterator.
		h.mock.On("ConnectionEvents").Return(func(yield func(ConnectionEvent) bool) {}).Maybe()
	}
}

// expectConnect sets up expectations for Connect calls.
func (h *testHarness) expectConnect(ctx context.Context) *testHarness {
	h.mock.On("Connect", ctx).Return(h.cfg.ConnectError).Once()
	return h
}

// expectConnectWithError sets up expectations for Connect to fail with error.
func (h *testHarness) expectConnectWithError(ctx context.Context, err error) *testHarness {
	h.mock.On("Connect", ctx).Return(err).Once()
	return h
}

// simulateConnectionStateChange updates IsConnected mock expectations to simulate state change.
func (h *testHarness) simulateConnectionStateChange(beforeCalls int, afterState bool) *testHarness {
	// Clear existing IsConnected expectations.
	h.mock.ExpectedCalls = filterExpectedCalls(h.mock.ExpectedCalls, "IsConnected")

	// Set up new expectations.
	h.mock.On("IsConnected").Return(!afterState).Times(beforeCalls)
	h.mock.On("IsConnected").Return(afterState)
	return h
}

// filterExpectedCalls removes expectations for a specific method.
func filterExpectedCalls(calls []*mock.Call, methodName string) []*mock.Call {
	var filtered []*mock.Call
	for _, call := range calls {
		if call.Method != methodName {
			filtered = append(filtered, call)
		}
	}
	return filtered
}

// expectSendMessage sets up expectations for SendMessage calls.
func (h *testHarness) expectSendMessage(msg lnwire.Message) *testHarness {
	h.mock.On("SendMessage", msg).Return(h.cfg.SendMessageError).Once()
	return h
}

// expectPongResponse sets up expectations for Pong responses to Ping messages.
func (h *testHarness) expectPongResponse(numPongBytes uint16) *testHarness {
	pongData := make([]byte, numPongBytes)
	expectedPong := lnwire.NewPong(pongData)
	h.mock.On("SendMessage", expectedPong).Return(nil).Once()
	return h
}

// getMock returns the underlying mock connection.
func (h *testHarness) getMock() *mockP2PConnection {
	return h.mock
}

// assertExpectations verifies all expectations were met.
func (h *testHarness) assertExpectations() {
	h.mock.AssertExpectations(h.t)
}

// NewTestKeyPair creates a new test key pair.
func NewTestKeyPair() (*btcec.PrivateKey, *btcec.PublicKey) {
	privKey, _ := btcec.NewPrivateKey()
	return privKey, privKey.PubKey()
}

// newTestPubKey creates a new test public key.
func newTestPubKey() *btcec.PublicKey {
	_, pubKey := NewTestKeyPair()
	return pubKey
}

// withActorSystem creates an actor system for the test.
func (h *testHarness) withActorSystem() *testHarness {
	h.system = actor.NewActorSystem()
	h.cleanup = append(h.cleanup, func() {
		h.system.Shutdown()
	})
	return h
}

// cleanupAll runs all cleanup functions.
func (h *testHarness) cleanupAll() {
	for _, cleanup := range h.cleanup {
		cleanup()
	}
}

// createPeerActor creates a PeerActor with the mock connection.
func (h *testHarness) createPeerActor(serviceKeys ...PeerServiceKey) *PeerActor {
	if h.system == nil {
		h.withActorSystem()
	}

	// Convert service keys to message sinks.
	sinks := make([]*MessageSink, len(serviceKeys))
	for i, key := range serviceKeys {
		sinks[i] = &MessageSink{ServiceKey: key}
	}

	cfg := PeerActorConfig{
		Connection:   h.mock,
		Receptionist: h.system.Receptionist(),
		MessageSinks: sinks,
		AutoConnect:  false,
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(h.t, err)

	// Register with the system and set actorRef so convenience methods work.
	serviceKey := actor.NewServiceKey[PeerMessage, PeerResponse]("test-harness-peer")
	actorRef := actor.RegisterWithSystem(h.system, "test-harness-peer", serviceKey, peerActor)
	peerActor.setActorRef(actorRef)

	return peerActor
}

// requireActorDisconnected verifies the actor's connection is disconnected.
func (h *testHarness) requireActorDisconnected(actor *PeerActor) *StatusResponse {
	ctx := context.Background()
	result := actor.Receive(ctx, &GetStatusRequest{})
	resp, err := result.Unpack()
	require.NoError(h.t, err)
	statusResp, ok := resp.(*StatusResponse)
	require.True(h.t, ok)
	require.False(h.t, statusResp.IsConnected)
	return statusResp
}

// requireConnectRequestAccepted checks that a ConnectRequest was accepted.
func (h *testHarness) requireConnectRequestAccepted(result fn.Result[PeerResponse]) *ConnectResponse {
	resp, err := result.Unpack()
	require.NoError(h.t, err)
	connResp, ok := resp.(*ConnectResponse)
	require.True(h.t, ok)
	require.True(h.t, connResp.Success) // Request was accepted
	return connResp
}

// requireConnectionFailed checks that a connection attempt failed via the Future.
func (h *testHarness) requireConnectionFailed(future actor.Future[ConnectionResult]) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := future.Await(ctx)
	connResult, err := result.Unpack()
	require.Error(h.t, err) // Connection should have failed
	require.False(h.t, connResult.Success)
}

// sendMessage sends a message through the actor.
func (h *testHarness) sendMessage(actor *PeerActor, msg lnwire.Message) error {
	ctx := context.Background()
	result := actor.Receive(ctx, &SendMessageRequest{
		Message: msg,
	})
	resp, err := result.Unpack()
	if err != nil {
		return err
	}
	sendResp, ok := resp.(*SendMessageResponse)
	if !ok {
		return fmt.Errorf("unexpected response type")
	}
	if !sendResp.Success {
		return fmt.Errorf("send failed")
	}
	return nil
}

// createServiceKey creates and registers a service key with a handler.
func (h *testHarness) createServiceKey(name string, handler func(context.Context, PeerMessage) fn.Result[PeerResponse]) PeerServiceKey {
	if h.system == nil {
		h.withActorSystem()
	}

	key := actor.NewServiceKey[PeerMessage, PeerResponse](name)
	behavior := actor.NewFunctionBehavior(handler)
	actor.RegisterWithSystem(h.system, name, key, behavior)
	return key
}

// registerServiceKey registers an existing service key with a handler.
func (h *testHarness) registerServiceKey(key PeerServiceKey, name string, handler func(context.Context, PeerMessage) fn.Result[PeerResponse]) {
	if h.system == nil {
		h.withActorSystem()
	}

	behavior := actor.NewFunctionBehavior(handler)
	actor.RegisterWithSystem(h.system, name, key, behavior)
}

// createPeerActorConfig creates a PeerActorConfig with common defaults.
func (h *testHarness) createPeerActorConfig(sinks ...*MessageSink) PeerActorConfig {
	if h.system == nil {
		h.withActorSystem()
	}

	return PeerActorConfig{
		Connection:   h.mock,
		Receptionist: h.system.Receptionist(),
		MessageSinks: sinks,
		AutoConnect:  false,
	}
}

// createMessageCounter creates a service key that counts received messages.
func (h *testHarness) createMessageCounter(name string) (PeerServiceKey, *int32) {
	var count int32
	key := h.createServiceKey(name, func(ctx context.Context, msg PeerMessage) fn.Result[PeerResponse] {
		if _, ok := msg.(*MessageReceived); ok {
			atomic.AddInt32(&count, 1)
		}
		return fn.Ok[PeerResponse](&MessageReceivedAck{Processed: true})
	})
	return key, &count
}

// startPeerActor starts the peer actor and verifies success.
func (h *testHarness) startPeerActor(actor *PeerActor) {
	ctx := context.Background()
	result := actor.Start(ctx)
	startResp, err := result.Unpack()
	require.NoError(h.t, err)
	require.True(h.t, startResp.Success)
}

// stopPeerActor stops the peer actor and verifies success.
func (h *testHarness) stopPeerActor(actor *PeerActor) {
	err := actor.Stop()
	require.NoError(h.t, err)
}

// addServiceKey adds a service key and verifies success.
func (h *testHarness) addServiceKey(actor *PeerActor, key PeerServiceKey) {
	added := actor.AddServiceKey(key)
	require.True(h.t, added)
}

// removeServiceKey removes a service key and verifies success.
func (h *testHarness) removeServiceKey(actor *PeerActor, key PeerServiceKey) {
	removed := actor.RemoveServiceKey(key)
	require.True(h.t, removed)
}

// requireServiceKeyCount verifies the number of service keys.
func (h *testHarness) requireServiceKeyCount(actor *PeerActor, expected int) {
	keys := actor.GetServiceKeys()
	require.Len(h.t, keys, expected)
}

