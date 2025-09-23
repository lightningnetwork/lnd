package lnp2p

import (
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/stretchr/testify/require"
)

// TestNodeAddressParsing tests the NodeAddress parsing functionality.
func TestNodeAddressParsing(t *testing.T) {
	// Test valid address.
	addrStr := "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735"
	addr, err := ParseNodeAddress(addrStr)
	require.NoError(t, err)
	require.NotNil(t, addr)
	require.NotNil(t, addr.PubKey)
	// net.ResolveTCPAddr will convert localhost to 127.0.0.1
	require.Equal(t, "127.0.0.1:9735", addr.Address.String())

	// Test roundtrip (note: localhost gets resolved to 127.0.0.1).
	addrStr2 := addr.String()
	expectedStr := "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@127.0.0.1:9735"
	require.Equal(t, expectedStr, addrStr2)

	// Test invalid formats.
	invalidAddrs := []string{
		"invalid",
		"@localhost:9735",
		"0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@",
		"notahex@localhost:9735",
	}

	for _, invalid := range invalidAddrs {
		_, err := ParseNodeAddress(invalid)
		require.Error(t, err, "should fail for: %s", invalid)
	}
}

// TestSimplePeerBroadcastModeRouting tests that SimplePeer routes messages to router in broadcast mode.
func TestSimplePeerBroadcastModeRouting(t *testing.T) {
	// Create keys for testing.
	localKey, _ := btcec.NewPrivateKey()
	remoteKey, _ := btcec.NewPrivateKey()

	// Create a mock router to track routed messages.
	mockRouter := &mockMsgRouter{}

	// Create a mock connection.
	mockConn := newMockBrontideConn(nil, localKey, remoteKey.PubKey())

	// Create test message to be read.
	testMsg := &lnwire.UpdateAddHTLC{ID: 123}

	// Add message to incoming queue.
	mockConn.incomingMessages <- testMsg

	// Expect RouteMsg to be called with our test message.
	expectedPeerMsg := msgmux.PeerMsg{
		Message: testMsg,
		PeerPub: *remoteKey.PubKey(),
	}
	mockRouter.On("RouteMsg", expectedPeerMsg).Return(nil).Once()
	mockRouter.On("Stop").Return().Maybe() // Called on Close()

	// Create SimplePeer with broadcast mode enabled.
	cfg := SimplePeerConfig{
		KeyGenerator: NewStaticKeyGenerator(&keychain.PrivKeyECDH{PrivKey: localKey}),
		Target: NodeAddress{
			PubKey:  remoteKey.PubKey(),
			Address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		},
		Features: DefaultTestFeatures(),
		Timeouts: fn.Some(DefaultTimeouts()),
	}

	peer, err := NewSimplePeer(cfg)
	require.NoError(t, err)

	// Set up the peer with our mock connection and router.
	peer.conn = mockConn
	peer.state.Store(int32(StateConnected))
	peer.EnableBroadcastMode(mockRouter)

	// Create channel to track message processing.
	messageSeen := make(chan struct{})

	// Monitor messages to know when it's been processed.
	go func() {
		for msg := range peer.ReceiveMessages() {
			if _, ok := msg.(*lnwire.UpdateAddHTLC); ok {
				close(messageSeen)
				return
			}
		}
	}()

	// Start the read handler.
	peer.wg.Add(1)
	go peer.readHandler()

	// Wait for message to be processed.
	select {
	case <-messageSeen:
		// Success - message was processed
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for message to be processed")
	}

	// Give a bit of time for the router to be called.
	time.Sleep(50 * time.Millisecond)

	// Clean up.
	peer.Close()

	// Verify the message was routed.
	mockRouter.AssertExpectations(t)
}

// TestTimeoutConfigs tests the timeout configuration functions.
func TestTimeoutConfigs(t *testing.T) {
	// Test default timeouts.
	defaults := DefaultTimeouts()
	require.Equal(t, 30*time.Second, defaults.DialTimeout)
	require.Equal(t, 15*time.Second, defaults.HandshakeTimeout)
	require.Equal(t, 1*time.Minute, defaults.PingInterval)

	// Test Tor timeouts.
	torTimeouts := TorTimeouts()
	require.Equal(t, defaults.DialTimeout*3, torTimeouts.DialTimeout)
	require.Equal(t, defaults.HandshakeTimeout*3, torTimeouts.HandshakeTimeout)
	require.Equal(t, defaults.PingInterval, torTimeouts.PingInterval) // Ping interval should not be multiplied.
}

// TestKeyGenerators tests the key generator implementations.
func TestKeyGenerators(t *testing.T) {
	// Test ephemeral key generator.
	ephGen := &EphemeralKeyGenerator{}
	key1, err := ephGen.GenerateKey()
	require.NoError(t, err)
	require.NotNil(t, key1)

	key2, err := ephGen.GenerateKey()
	require.NoError(t, err)
	require.NotNil(t, key2)

	// Keys should be different.
	require.NotEqual(t, key1.PubKey().SerializeCompressed(),
		key2.PubKey().SerializeCompressed())

	// Test static key generator.
	staticGen := NewStaticKeyGenerator(key1)
	key3, err := staticGen.GenerateKey()
	require.NoError(t, err)
	require.Equal(t, key1.PubKey().SerializeCompressed(),
		key3.PubKey().SerializeCompressed())

	key4, err := staticGen.GenerateKey()
	require.NoError(t, err)
	require.Equal(t, key1.PubKey().SerializeCompressed(),
		key4.PubKey().SerializeCompressed())
}

// TestConnectionStates tests the connection state string representations.
func TestConnectionStates(t *testing.T) {
	tests := []struct {
		state    ConnectionState
		expected string
	}{
		{StateDisconnected, "disconnected"},
		{StateConnecting, "connecting"},
		{StateHandshaking, "handshaking"},
		{StateInitializing, "initializing"},
		{StateConnected, "connected"},
		{StateClosing, "closing"},
		{ConnectionState(999), "unknown"},
	}

	for _, test := range tests {
		require.Equal(t, test.expected, test.state.String())
	}
}

// TestSimplePeerCreation tests creating a SimplePeer instance.
func TestSimplePeerCreation(t *testing.T) {
	target, err := ParseNodeAddress("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735")
	require.NoError(t, err)

	cfg := SimplePeerConfig{
		KeyGenerator: &EphemeralKeyGenerator{},
		Target:       *target,
		Features:     DefaultTestFeatures(),
		Timeouts:     fn.Some(DefaultTimeouts()),
		Dialer:       fn.Some(DefaultTestDialer()),
	}

	peer, err := NewSimplePeer(cfg)
	require.NoError(t, err)
	require.NotNil(t, peer)

	// Test that defaults are set.
	require.NotNil(t, peer.cfg.Features)
	require.NotNil(t, peer.timeouts)
	require.False(t, peer.IsConnected())
}

// TestDefaultTestFeatures tests the default test features.
func TestDefaultTestFeatures(t *testing.T) {
	features := DefaultTestFeatures()
	require.NotNil(t, features)

	// Check that some common features are set.
	expectedFeatures := []lnwire.FeatureBit{
		lnwire.DataLossProtectOptional,
		lnwire.GossipQueriesOptional,
		lnwire.TLVOnionPayloadOptional,
		lnwire.StaticRemoteKeyOptional,
		lnwire.UpfrontShutdownScriptOptional,
	}

	for _, bit := range expectedFeatures {
		require.True(t, features.HasFeature(bit),
			"feature %v should be set", bit)
	}
}

// TestPeerActorMessages tests the actor message types.
func TestPeerActorMessages(t *testing.T) {
	// Test message type names.
	msgs := []struct {
		msg      PeerMessage
		expected string
	}{
		{&ConnectRequest{}, "ConnectRequest"},
		{&DisconnectRequest{}, "DisconnectRequest"},
		{&SendMessageRequest{}, "SendMessageRequest"},
		{&GetStatusRequest{}, "GetStatusRequest"},
		{&WaitForConnectionRequest{}, "WaitForConnectionRequest"},
		{&MessageReceived{}, "MessageReceived"},
		{&ConnectionStateChange{}, "ConnectionStateChange"},
		{&PeerError{}, "PeerError"},
		{&PeerWarning{}, "PeerWarning"},
		{&AddServiceKeyRequest{}, "AddServiceKeyRequest"},
		{&RemoveServiceKeyRequest{}, "RemoveServiceKeyRequest"},
		{&GetServiceKeysRequest{}, "GetServiceKeysRequest"},
	}

	for _, test := range msgs {
		require.Equal(t, test.expected, test.msg.MessageType())
	}
}

// TestErrorTypes tests the error type constants.
func TestErrorTypes(t *testing.T) {
	// Just ensure the constants are defined.
	require.Equal(t, ErrorType(0), ErrorTypeConnection)
	require.Equal(t, ErrorType(1), ErrorTypeProtocol)
	require.Equal(t, ErrorType(2), ErrorTypeMessage)
	require.Equal(t, ErrorType(3), ErrorTypeTimeout)
	require.Equal(t, ErrorType(4), ErrorTypeRemote)
}

// TestPeerActorConfigValidation tests the PeerActor config validation.
func TestPeerActorConfigValidation(t *testing.T) {
	// Test nil connection.
	cfg := PeerActorConfig{
		Connection:   nil,
		Receptionist: &actor.Receptionist{},
	}
	err := ValidatePeerActorConfig(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection is required")

	// Test nil receptionist.
	mockConn := new(mockP2PConnection)
	cfg = PeerActorConfig{
		Connection:   mockConn,
		Receptionist: nil,
	}
	err = ValidatePeerActorConfig(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "receptionist is required")

	// Test valid config (ServiceKeys can be empty).
	cfg = PeerActorConfig{
		Connection:   mockConn,
		Receptionist: &actor.Receptionist{},
		MessageSinks: []*MessageSink{},
	}
	err = ValidatePeerActorConfig(cfg)
	require.NoError(t, err)
}

// TestSimplePeerEnableBroadcastMode tests the EnableBroadcastMode functionality.
func TestSimplePeerEnableBroadcastMode(t *testing.T) {
	// Create a simple peer.
	serverKey, _ := btcec.NewPrivateKey()
	target := NodeAddress{
		PubKey:  serverKey.PubKey(),
		Address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
	}

	cfg := SimplePeerConfig{
		KeyGenerator: &EphemeralKeyGenerator{},
		Target:       target,
		Features:     DefaultTestFeatures(),
		Timeouts:     fn.Some(DefaultTimeouts()),
	}

	peer, err := NewSimplePeer(cfg)
	require.NoError(t, err)
	defer peer.Close()

	// Create a mock router.
	mockRouter := &mockMsgRouter{}
	mockRouter.On("Stop").Return().Maybe() // Called on Close()

	// Initially, broadcast mode should be disabled.
	require.False(t, peer.broadcastMode.Load())
	require.False(t, peer.msgRouter.IsSome())

	// Enable broadcast mode.
	peer.EnableBroadcastMode(mockRouter)

	// Verify broadcast mode is enabled.
	require.True(t, peer.broadcastMode.Load())
	require.True(t, peer.msgRouter.IsSome())

	// Verify the router was set correctly.
	peer.msgRouter.WhenSome(func(r msgmux.Router) {
		require.Equal(t, mockRouter, r)
	})
}
