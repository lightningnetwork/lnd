package lnp2p

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// createTestPeerActorWithRef is a test helper that creates a PeerActor with its
// actorRef properly set up. This ensures convenience methods work through the
// actor system, helping us confirm there are no race conditions.
func createTestPeerActorWithRef(t *testing.T, system *actor.ActorSystem,
	cfg PeerActorConfig) (*PeerActor, actor.ActorRef[PeerMessage, PeerResponse]) {

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Register with the system to get an actorRef.
	serviceKey := actor.NewServiceKey[PeerMessage, PeerResponse]("test-peer")
	actorRef := actor.RegisterWithSystem(system, "test-peer", serviceKey, peerActor)

	// Set the actorRef so convenience methods work through the actor system.
	peerActor.setActorRef(actorRef)

	return peerActor, actorRef
}

// TestCreatePeerServiceWithNilReceptionist tests that CreatePeerService
// properly handles nil Receptionist.
func TestCreatePeerServiceWithNilReceptionist(t *testing.T) {
	system := actor.NewActorSystem()
	defer system.Shutdown()

	mockConn := &mockP2PConnection{}
	mockConn.On("RemotePubKey").Return(nil)
	mockConn.On("RemoteAddr").Return(nil)

	cfg := PeerActorConfig{
		Connection:   mockConn,
		Receptionist: nil,
	}

	_, err := CreatePeerService(system, "test-peer", cfg)
	require.NoError(t, err)
}

// TestHandleAddServiceKeyWithNilMessageSink tests that handleAddServiceKey
// properly handles nil MessageSink.
func TestHandleAddServiceKeyWithNilMessageSink(t *testing.T) {
	ctx := context.Background()
	system := actor.NewActorSystem()
	defer system.Shutdown()

	mockConn := &mockP2PConnection{}
	mockConn.On("RemotePubKey").Return(nil)
	mockConn.On("RemoteAddr").Return(nil)

	cfg := PeerActorConfig{
		Connection:   mockConn,
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	result := peerActor.Receive(ctx, &AddServiceKeyRequest{
		MessageSink: nil,
	})

	resp, err := result.Unpack()
	require.NoError(t, err)

	addResp, ok := resp.(*AddServiceKeyResponse)
	require.True(t, ok)
	require.False(t, addResp.Success)
}

// TestNewActorWithConn tests the NewActorWithConn high-level constructor.
func TestNewActorWithConn(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create test keys.
	privKey, _ := btcec.NewPrivateKey()
	pubKey := privKey.PubKey()

	// Create config.
	cfg := ActorWithConnConfig{
		SimplePeerCfg: SimplePeerConfig{
			KeyGenerator: NewStaticKeyGenerator(&keychain.PrivKeyECDH{PrivKey: privKey}),
			Target: NodeAddress{
				PubKey:  pubKey,
				Address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
			},
			Features: lnwire.NewFeatureVector(lnwire.NewRawFeatureVector(), nil),
			// Use None for dialer to use default net.Dial which will fail to connect to test address.
		},
		ActorSystem: system,
		ServiceKey:  actor.NewServiceKey[PeerMessage, PeerResponse]("test-peer"),
		ActorName:   "test-peer-actor",
		MessageSinks: []*MessageSink{
			{
				ServiceKey: actor.NewServiceKey[PeerMessage, PeerResponse]("test-sink"),
			},
		},
		AutoConnect: false,
	}

	// Create actor with connection.
	peerActor, actorRef, err := NewActorWithConn(cfg)
	require.NoError(t, err)
	require.NotNil(t, peerActor)
	require.NotNil(t, actorRef)

	// Verify the actor is properly set up using convenience methods.
	// Note: We could use raw requests via actorRef.Ask(), but the convenience
	// methods are simpler and still go through the actor system.
	statusResp := peerActor.GetStatus()
	require.NotNil(t, statusResp)
	require.False(t, statusResp.IsConnected)

	// Verify service keys are set using convenience method.
	sinks := peerActor.GetMessageSinks()
	require.Len(t, sinks, 1)

	// Clean up.
	err = peerActor.Stop()
	require.NoError(t, err)
}

// TestPeerActorCreation tests creating a PeerActor.
func TestPeerActorCreation(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create test harness with default mock.
	harness := newTestHarness(t, nil)

	// Test creation with valid config.
	cfg := PeerActorConfig{
		Connection:   harness.getMock(),
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)
	require.NotNil(t, peerActor)

	// Test creation with invalid config (nil connection).
	badCfg := PeerActorConfig{
		Connection:   nil,
		Receptionist: system.Receptionist(),
	}
	_, err = NewPeerActor(badCfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection is required")

	// Test creation with invalid config (nil receptionist).
	badCfg2 := PeerActorConfig{
		Connection:   harness.getMock(),
		Receptionist: nil,
	}
	_, err = NewPeerActor(badCfg2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "receptionist is required")
}

// TestPeerActorReceive tests the Receive method for various message types.
func TestPeerActorReceive(t *testing.T) {
	ctx := context.Background()

	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create mock connection with basic expectations.
	mockConn := &mockP2PConnection{}
	mockConn.On("RemotePubKey").Return((*btcec.PublicKey)(nil)).Maybe()
	mockConn.On("RemoteAddr").Return(net.Addr(nil)).Maybe()
	mockConn.On("IsConnected").Return(false)
	mockConn.On("RemotePubKey").Return(nil)
	mockConn.On("LocalPubKey").Return(nil)
	mockConn.On("Close").Return(nil)

	cfg := PeerActorConfig{
		Connection:   mockConn,
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
		AutoConnect:  false,
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Test GetStatusRequest.
	statusResult := peerActor.Receive(ctx, &GetStatusRequest{})
	resp, err := statusResult.Unpack()
	require.NoError(t, err)
	statusResp, ok := resp.(*StatusResponse)
	require.True(t, ok)
	require.False(t, statusResp.IsConnected)

	// Test WaitForConnectionRequest when not connected.
	waitResult := peerActor.Receive(ctx, &WaitForConnectionRequest{})
	resp, err = waitResult.Unpack()
	require.NoError(t, err)
	waitResp, ok := resp.(*WaitForConnectionResponse)
	require.True(t, ok)
	require.NotNil(t, waitResp.Future)
	require.False(t, waitResp.AlreadyConnected)

	// Test DisconnectRequest.
	disconnectResult := peerActor.Receive(ctx, &DisconnectRequest{})
	resp, err = disconnectResult.Unpack()
	require.NoError(t, err)
	disconnectResp, ok := resp.(*DisconnectResponse)
	require.True(t, ok)
	require.True(t, disconnectResp.Success)

	// Test SendMessageRequest when not connected.
	// IsConnected will return false, so SendMessage should fail.
	sendResult := peerActor.Receive(ctx, &SendMessageRequest{
		Message: lnwire.NewPing(100),
	})
	_, err = sendResult.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")

	// Test unknown message type.
	unknownResult := peerActor.Receive(ctx, &mockUnknownMessage{})
	_, err = unknownResult.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown message type")
}

// TestPeerActorDirectMethods tests the direct methods on PeerActor.
func TestPeerActorDirectMethods(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create mock connection with basic expectations.
	mockConn := &mockP2PConnection{}
	mockConn.On("RemotePubKey").Return((*btcec.PublicKey)(nil)).Maybe()
	mockConn.On("RemoteAddr").Return(net.Addr(nil)).Maybe()

	cfg := PeerActorConfig{
		Connection:   mockConn,
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
	}

	// Use CreatePeerService to properly set up the actor with its reference.
	actorRef, err := CreatePeerService(system, "test-peer", cfg)
	require.NoError(t, err)

	// For testing convenience methods, we need to get the underlying PeerActor.
	// Since we can't access it directly, we'll test through the actorRef.
	ctx := context.Background()

	// Test GetServiceKeys - should be empty.
	future := actorRef.Ask(ctx, &GetServiceKeysRequest{})
	result := future.Await(ctx)
	resp, err := result.Unpack()
	require.NoError(t, err)
	keysResp := resp.(*GetServiceKeysResponse)
	require.Len(t, keysResp.ServiceKeys, 0)

	// Test AddServiceKey.
	testKey := actor.NewServiceKey[PeerMessage, PeerResponse]("test-key-2")
	addFuture := actorRef.Ask(ctx, &AddServiceKeyRequest{
		MessageSink: &MessageSink{
			ServiceKey: testKey,
		},
	})
	addResult := addFuture.Await(ctx)
	addResp, err := addResult.Unpack()
	require.NoError(t, err)
	addResponse := addResp.(*AddServiceKeyResponse)
	require.True(t, addResponse.Success)

	// Adding again should fail (already exists).
	addFuture2 := actorRef.Ask(ctx, &AddServiceKeyRequest{
		MessageSink: &MessageSink{
			ServiceKey: testKey,
		},
	})
	addResult2 := addFuture2.Await(ctx)
	addResp2, err := addResult2.Unpack()
	require.NoError(t, err)
	addResponse2 := addResp2.(*AddServiceKeyResponse)
	require.False(t, addResponse2.Success)
	require.True(t, addResponse2.AlreadyExists)

	// Check that key was added.
	future = actorRef.Ask(ctx, &GetServiceKeysRequest{})
	result = future.Await(ctx)
	resp, err = result.Unpack()
	require.NoError(t, err)
	keysResp = resp.(*GetServiceKeysResponse)
	require.Len(t, keysResp.ServiceKeys, 1)

	// Test RemoveServiceKey.
	removeFuture := actorRef.Ask(ctx, &RemoveServiceKeyRequest{
		ServiceKey: testKey,
	})
	removeResult := removeFuture.Await(ctx)
	removeResp, err := removeResult.Unpack()
	require.NoError(t, err)
	removeResponse := removeResp.(*RemoveServiceKeyResponse)
	require.True(t, removeResponse.Success)

	// Removing again should fail.
	removeFuture2 := actorRef.Ask(ctx, &RemoveServiceKeyRequest{
		ServiceKey: testKey,
	})
	removeResult2 := removeFuture2.Await(ctx)
	removeResp2, err := removeResult2.Unpack()
	require.NoError(t, err)
	removeResponse2 := removeResp2.(*RemoveServiceKeyResponse)
	require.False(t, removeResponse2.Success)
	require.True(t, removeResponse2.NotFound)

	// Check that key was removed.
	future = actorRef.Ask(ctx, &GetServiceKeysRequest{})
	result = future.Await(ctx)
	resp, err = result.Unpack()
	require.NoError(t, err)
	keysResp = resp.(*GetServiceKeysResponse)
	require.Len(t, keysResp.ServiceKeys, 0)
}

// TestPeerActorConnectionFuture tests the ConnectionFuture method.
func TestPeerActorConnectionFuture(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create mock connection with basic expectations.
	mockConn := &mockP2PConnection{}
	mockConn.On("RemotePubKey").Return((*btcec.PublicKey)(nil)).Maybe()
	mockConn.On("RemoteAddr").Return(net.Addr(nil)).Maybe()

	cfg := PeerActorConfig{
		Connection:   mockConn,
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Get the connection future.
	future := peerActor.ConnectionFuture()
	require.NotNil(t, future)
}

// TestPeerActorStart tests the Start method.
func TestPeerActorStart(t *testing.T) {
	ctx := context.Background()

	// Test with AutoConnect = false.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	mockConn := &mockP2PConnection{}
	// Set up expectations for the mock.
	mockConn.On("IsConnected").Return(false)
	mockConn.On("RemotePubKey").Return((*btcec.PublicKey)(nil)).Maybe()
	mockConn.On("RemoteAddr").Return(net.Addr(nil)).Maybe()
	mockConn.On("ConnectionEvents").Return(func(yield func(ConnectionEvent) bool) {})
	mockConn.On("ReceiveMessages").Return(func(yield func(lnwire.Message) bool) {})

	cfg := PeerActorConfig{
		Connection:   mockConn,
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
		AutoConnect:  false,
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	result := peerActor.Start(ctx)
	connResult, err := result.Unpack()
	require.NoError(t, err)
	require.True(t, connResult.Success)

	// Test with AutoConnect = true.
	mockConn2 := &mockP2PConnection{}
	mockConn2.On("IsConnected").Return(false)
	mockConn2.On("Connect", ctx).Return(nil)
	mockConn2.On("RemotePubKey").Return(nil)
	mockConn2.On("ConnectionEvents").Return(func(yield func(ConnectionEvent) bool) {})
	mockConn2.On("ReceiveMessages").Return(func(yield func(lnwire.Message) bool) {})

	cfg2 := PeerActorConfig{
		Connection:   mockConn2,
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
		AutoConnect:  true,
	}

	peerActor2, err := NewPeerActor(cfg2)
	require.NoError(t, err)

	// This will try to connect and our mock returns success.
	result2 := peerActor2.Start(ctx)
	connResult2, err := result2.Unpack()
	require.NoError(t, err)
	// Since Connect returns nil (success), this should be true.
	require.True(t, connResult2.Success)
}

// TestPeerActorStop tests the Stop method.
func TestPeerActorStop(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	mockConn := &mockP2PConnection{}
	// Set up expectations for Close method and logging.
	mockConn.On("RemotePubKey").Return((*btcec.PublicKey)(nil)).Maybe()
	mockConn.On("RemoteAddr").Return(net.Addr(nil)).Maybe()
	mockConn.On("Close").Return(nil)

	cfg := PeerActorConfig{
		Connection:   mockConn,
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Stop should not error.
	err = peerActor.Stop()
	require.NoError(t, err)

	// Verify the mock expectations were met.
	mockConn.AssertExpectations(t)
}

// TestPeerActorGetMessageSinks tests the GetMessageSinks convenience method.
func TestPeerActorGetMessageSinks(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create test harness.
	harness := newTestHarness(t, nil)
	harness.withActorSystem()

	// Create service keys and filters.
	key1 := actor.NewServiceKey[PeerMessage, PeerResponse]("filtered-key")
	key2 := actor.NewServiceKey[PeerMessage, PeerResponse]("unfiltered-key")

	testFilter := func(msg lnwire.Message) bool {
		return true
	}

	// Configure peer actor with mixed sinks.
	cfg := PeerActorConfig{
		Connection:   harness.getMock(),
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{
			{ServiceKey: key1, Filter: testFilter},
			{ServiceKey: key2, Filter: nil},
		},
		AutoConnect: false,
	}

	// Use helper to create actor with actorRef properly set.
	peerActor, _ := createTestPeerActorWithRef(t, system, cfg)

	// Get message sinks using convenience method (goes through actor system).
	sinks := peerActor.GetMessageSinks()
	require.Len(t, sinks, 2)

	// Verify the sinks match what we configured.
	require.Equal(t, key1, sinks[0].ServiceKey)
	require.NotNil(t, sinks[0].Filter)
	require.Equal(t, key2, sinks[1].ServiceKey)
	require.Nil(t, sinks[1].Filter)

	// Add another sink dynamically.
	key3 := actor.NewServiceKey[PeerMessage, PeerResponse]("dynamic-key")
	added := peerActor.AddMessageSink(&MessageSink{
		ServiceKey: key3,
		Filter:     testFilter,
	})
	require.True(t, added)

	// Get sinks again.
	sinks = peerActor.GetMessageSinks()
	require.Len(t, sinks, 3)
}

// TestMessageSinkShouldReceive tests the ShouldReceive method directly.
func TestMessageSinkShouldReceive(t *testing.T) {
	// Test with no filter - should accept all.
	sink1 := &MessageSink{
		ServiceKey: actor.NewServiceKey[PeerMessage, PeerResponse]("test"),
		Filter:     nil,
	}
	require.True(t, sink1.ShouldReceive(lnwire.NewPing(0)))
	require.True(t, sink1.ShouldReceive(lnwire.NewPong(nil)))

	// Test with filter that only accepts Ping.
	sink2 := &MessageSink{
		ServiceKey: actor.NewServiceKey[PeerMessage, PeerResponse]("test"),
		Filter: func(msg lnwire.Message) bool {
			_, isPing := msg.(*lnwire.Ping)
			return isPing
		},
	}
	require.True(t, sink2.ShouldReceive(lnwire.NewPing(0)))
	require.False(t, sink2.ShouldReceive(lnwire.NewPong(nil)))

	// Test with filter that rejects all.
	sink3 := &MessageSink{
		ServiceKey: actor.NewServiceKey[PeerMessage, PeerResponse]("test"),
		Filter: func(msg lnwire.Message) bool {
			return false
		},
	}
	require.False(t, sink3.ShouldReceive(lnwire.NewPing(0)))
	require.False(t, sink3.ShouldReceive(lnwire.NewPong(nil)))
}

// TestPeerActorHandleRemoveServiceKeyRebuild tests handleRemoveServiceKey
// rebuilding newSinks.
func TestPeerActorHandleRemoveServiceKeyRebuild(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create test harness.
	harness := newTestHarness(t, nil)
	harness.withActorSystem()

	ctx := context.Background()

	// Create service keys.
	key1 := actor.NewServiceKey[PeerMessage, PeerResponse]("key-1")
	key2 := actor.NewServiceKey[PeerMessage, PeerResponse]("key-2")
	key3 := actor.NewServiceKey[PeerMessage, PeerResponse]("key-3")

	// Configure peer actor with multiple sinks including duplicates.
	cfg := PeerActorConfig{
		Connection:   harness.getMock(),
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{
			{ServiceKey: key1},
			{ServiceKey: key2},
			// Duplicate key1.
			{ServiceKey: key1},
			{ServiceKey: key3},
			// Duplicate key2.
			{ServiceKey: key2},
		},
		AutoConnect: false,
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Get initial service keys to verify starting state.
	getKeysReq := &GetServiceKeysRequest{}
	result := peerActor.Receive(ctx, getKeysReq)
	keysResp, err := result.Unpack()
	require.NoError(t, err)
	initialKeys := keysResp.(*GetServiceKeysResponse)
	// Should have 3 unique keys even though 5 sinks.
	require.Len(t, initialKeys.ServiceKeys, 3)

	// Remove key2 through the handler - should remove both key2 sinks.
	removeReq := &RemoveServiceKeyRequest{ServiceKey: key2}
	removeResult := peerActor.Receive(ctx, removeReq)
	removeResp, err := removeResult.Unpack()
	require.NoError(t, err)
	resp := removeResp.(*RemoveServiceKeyResponse)
	require.True(t, resp.Success)
	require.False(t, resp.NotFound)
	// 2 unique keys remain.
	require.Equal(t, 2, resp.CurrentKeyCount)

	// Verify the correct keys remain.
	getKeysReq = &GetServiceKeysRequest{}
	result = peerActor.Receive(ctx, getKeysReq)
	keysResp, err = result.Unpack()
	require.NoError(t, err)
	finalKeys := keysResp.(*GetServiceKeysResponse)
	require.Len(t, finalKeys.ServiceKeys, 2)

	keyStrs := make(map[string]bool)
	for _, k := range finalKeys.ServiceKeys {
		keyStrs[fmt.Sprintf("%v", k)] = true
	}
	require.True(t, keyStrs[fmt.Sprintf("%v", key1)])
	require.True(t, keyStrs[fmt.Sprintf("%v", key3)])
	require.False(t, keyStrs[fmt.Sprintf("%v", key2)])
}

// TestPeerActorRefreshActorRefsNilSetting tests refreshActorRefs setting refs
// to nil.
func TestPeerActorRefreshActorRefsNilSetting(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create test harness.
	harness := newTestHarness(t, nil)
	harness.withActorSystem()

	// Create service keys.
	key1 := actor.NewServiceKey[PeerMessage, PeerResponse]("handler-1")
	key2 := actor.NewServiceKey[PeerMessage, PeerResponse]("handler-2")

	// Initially don't register any actors.
	cfg := PeerActorConfig{
		Connection:   harness.getMock(),
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{
			{ServiceKey: key1},
			{ServiceKey: key2},
		},
		AutoConnect: false,
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Both sinks should have nil refs initially.
	require.Nil(t, peerActor.messageSinks[0].actorRef)
	require.Nil(t, peerActor.messageSinks[1].actorRef)

	// Register actor for key2 only.
	actor.RegisterWithSystem(system, "handler-2", key2, actor.NewFunctionBehavior(
		func(_ context.Context, _ PeerMessage) fn.Result[PeerResponse] {
			return fn.Ok[PeerResponse](nil)
		},
	))

	// Refresh actor refs.
	peerActor.refreshActorRefs()

	// Verify key1's ref is still nil, key2's ref is now set.
	require.Nil(t, peerActor.messageSinks[0].actorRef)
	require.NotNil(t, peerActor.messageSinks[1].actorRef)

	// Now register actor for key1.
	actor.RegisterWithSystem(system, "handler-1", key1, actor.NewFunctionBehavior(
		func(_ context.Context, _ PeerMessage) fn.Result[PeerResponse] {
			return fn.Ok[PeerResponse](nil)
		},
	))

	// Refresh again.
	peerActor.refreshActorRefs()

	// Now both should have refs.
	require.NotNil(t, peerActor.messageSinks[0].actorRef)
	require.NotNil(t, peerActor.messageSinks[1].actorRef)
}

// TestPeerActorAutoConnectAlreadyConnected tests AutoConnect when already
// connected.
func TestPeerActorAutoConnectAlreadyConnected(t *testing.T) {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create test harness with connected config.
	harness := newTestHarness(t, ConnectedMockConfig())
	harness.withActorSystem()

	ctx := context.Background()

	// Configure peer actor with AutoConnect.
	cfg := PeerActorConfig{
		Connection:   harness.getMock(),
		Receptionist: system.Receptionist(),
		MessageSinks: []*MessageSink{},
		AutoConnect:  true,
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Start with AutoConnect - should detect already connected.
	result := peerActor.Start(ctx)
	connResult, err := result.Unpack()
	require.NoError(t, err)
	require.True(t, connResult.Success)
	require.NotNil(t, connResult.RemotePubKey)

	// Stop the actor.
	err = peerActor.Stop()
	require.NoError(t, err)
}

