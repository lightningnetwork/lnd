package lnp2p

import (
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// DefaultTestFeatures returns default feature bits for testing.
func DefaultTestFeatures() *lnwire.FeatureVector {
	rawFeatures := lnwire.NewRawFeatureVector()

	// Set common features for testing.
	rawFeatures.Set(lnwire.DataLossProtectOptional)
	rawFeatures.Set(lnwire.GossipQueriesOptional)
	rawFeatures.Set(lnwire.TLVOnionPayloadOptional)
	rawFeatures.Set(lnwire.StaticRemoteKeyOptional)
	rawFeatures.Set(lnwire.UpfrontShutdownScriptOptional)

	// Convert to FeatureVector.
	return lnwire.NewFeatureVector(rawFeatures, nil)
}

// DefaultTestDialer returns a default TCP dialer for testing.
func DefaultTestDialer() tor.DialFunc {
	return func(network, address string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout(network, address, timeout)
	}
}

// createMockPeerWithDialer creates a SimplePeer with a mock brontide dialer.
// This helper reduces duplication when setting up tests that need to mock
// the connection establishment.
func createMockPeerWithDialer(clientKey *btcec.PrivateKey, serverKey *btcec.PublicKey,
	mockConn BrontideConn) (*SimplePeer, error) {

	target := NodeAddress{
		PubKey:  serverKey,
		Address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
	}

	// Create mock brontide dialer that returns the provided connection.
	mockDialer := NewMockBrontideDialer(
		func(localKey keychain.SingleKeyECDH, netAddr *lnwire.NetAddress,
			timeout time.Duration, dialer tor.DialFunc) (BrontideConn, error) {
			return mockConn, nil
		},
	)

	cfg := SimplePeerConfig{
		KeyGenerator:   NewStaticKeyGenerator(&keychain.PrivKeyECDH{PrivKey: clientKey}),
		Target:         target,
		Features:       DefaultTestFeatures(),
		Timeouts:       fn.Some(DefaultTimeouts()),
		BrontideDialer: fn.Some[BrontideDialer](mockDialer),
	}

	return NewSimplePeer(cfg)
}

// setupMockPeerConnection creates a connected SimplePeer with mock connections.
// This is useful for tests that need a fully connected peer without going
// through the actual connection process.
func setupMockPeerConnection(t *testing.T) (*SimplePeer, *mockBrontideConn, *mockBrontideConn) {
	// Create keys.
	clientKey, _ := btcec.NewPrivateKey()
	serverKey, _ := btcec.NewPrivateKey()

	// Create mock connections.
	clientConn, serverConn, err := createMockBrontidePairWithKeys(clientKey, serverKey)
	require.NoError(t, err)

	// Create peer with mock dialer.
	peer, err := createMockPeerWithDialer(clientKey, serverKey.PubKey(), clientConn)
	require.NoError(t, err)

	// Queue server's init message for the init exchange.
	serverFeatures := DefaultTestFeatures()
	serverInit := &lnwire.Init{
		GlobalFeatures: serverFeatures.RawFeatureVector,
		Features:       serverFeatures.RawFeatureVector,
	}
	clientConn.QueueIncomingMessage(serverInit)

	return peer, clientConn, serverConn
}

