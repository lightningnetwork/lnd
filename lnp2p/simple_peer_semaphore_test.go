package lnp2p

import (
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestReadSemaphoreBlocking tests that the read semaphore blocks reads when no tokens are available.
func TestReadSemaphoreBlocking(t *testing.T) {
	// Create keys.
	clientKey, _ := btcec.NewPrivateKey()
	serverKey, _ := btcec.NewPrivateKey()

	// Create mock connections.
	clientConn, serverConn, err := createMockBrontidePairWithKeys(clientKey, serverKey)
	require.NoError(t, err)

	// Create a read semaphore channel with no tokens.
	readSem := make(chan struct{}, 1)

	// Create peer with read semaphore.
	cfg := SimplePeerConfig{
		KeyGenerator: NewStaticKeyGenerator(&keychain.PrivKeyECDH{PrivKey: clientKey}),
		Target: NodeAddress{
			PubKey:  serverKey.PubKey(),
			Address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		},
		Features:      DefaultTestFeatures(),
		Timeouts:      fn.Some(DefaultTimeouts()),
		ReadSemaphore: fn.Some(readSem),
	}

	peer, err := NewSimplePeer(cfg)
	require.NoError(t, err)

	// Set the connection and mark as connected.
	peer.conn = clientConn
	peer.state.Store(int32(StateConnected))

	// Queue server's init message.
	serverInit := &lnwire.Init{
		GlobalFeatures: DefaultTestFeatures().RawFeatureVector,
		Features:       DefaultTestFeatures().RawFeatureVector,
	}
	clientConn.QueueIncomingMessage(serverInit)

	// Queue a regular message.
	testMsg := &lnwire.UpdateAddHTLC{ID: 123}
	clientConn.QueueIncomingMessage(testMsg)

	// Start the read handler.
	peer.wg.Add(1)
	go peer.readHandler()

	// Create channel to track received messages.
	messageCount := 0
	done := make(chan struct{})
	go func() {
		for range peer.ReceiveMessages() {
			messageCount++
			if messageCount == 1 {
				// Got the first message, signal done.
				close(done)
			}
		}
	}()

	// Wait a bit - no messages should be received yet.
	select {
	case <-done:
		t.Fatal("Should not have received any messages without semaphore token")
	case <-time.After(100 * time.Millisecond):
		// Expected - no messages received.
	}

	// Send one token to allow one read.
	readSem <- struct{}{}

	// Now we should receive exactly one message.
	select {
	case <-done:
		// Success - got the message.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for message after sending semaphore token")
	}

	// Verify we got exactly one message.
	require.Equal(t, 1, messageCount)

	// Clean up.
	peer.Close()

	// Verify server connection also processed init.
	select {
	case msg := <-serverConn.outgoingMessages:
		_, ok := msg.(*lnwire.Init)
		require.True(t, ok, "Expected Init message")
	case <-time.After(100 * time.Millisecond):
		// Server may not have received init if we closed quickly.
	}
}

// TestReadSemaphoreMultipleTokens tests that the read semaphore allows exactly N reads for N tokens.
func TestReadSemaphoreMultipleTokens(t *testing.T) {
	// Create keys.
	clientKey, _ := btcec.NewPrivateKey()
	serverKey, _ := btcec.NewPrivateKey()

	// Create mock connections.
	clientConn, _, err := createMockBrontidePairWithKeys(clientKey, serverKey)
	require.NoError(t, err)

	// Create a buffered read semaphore channel.
	readSem := make(chan struct{}, 5)

	// Create peer with read semaphore.
	cfg := SimplePeerConfig{
		KeyGenerator: NewStaticKeyGenerator(&keychain.PrivKeyECDH{PrivKey: clientKey}),
		Target: NodeAddress{
			PubKey:  serverKey.PubKey(),
			Address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		},
		Features:      DefaultTestFeatures(),
		Timeouts:      fn.Some(DefaultTimeouts()),
		ReadSemaphore: fn.Some(readSem),
	}

	peer, err := NewSimplePeer(cfg)
	require.NoError(t, err)

	// Set the connection and mark as connected.
	peer.conn = clientConn
	peer.state.Store(int32(StateConnected))

	// Queue multiple messages.
	for i := 0; i < 5; i++ {
		testMsg := &lnwire.UpdateAddHTLC{ID: uint64(i)}
		clientConn.QueueIncomingMessage(testMsg)
	}

	// Track received messages.
	messageCount := 0
	messageChan := make(chan lnwire.Message, 5)
	go func() {
		for msg := range peer.ReceiveMessages() {
			messageCount++
			messageChan <- msg
		}
	}()

	// Start the read handler.
	peer.wg.Add(1)
	go peer.readHandler()

	// Send 3 tokens - should allow exactly 3 reads.
	for i := 0; i < 3; i++ {
		readSem <- struct{}{}
	}

	// Wait for 3 messages.
	time.Sleep(200 * time.Millisecond)

	// Should have received exactly 3 messages.
	require.Equal(t, 3, messageCount)

	// Send 2 more tokens.
	for i := 0; i < 2; i++ {
		readSem <- struct{}{}
	}

	// Wait for 2 more messages.
	time.Sleep(200 * time.Millisecond)

	// Should have received exactly 5 messages total.
	require.Equal(t, 5, messageCount)

	// Clean up.
	peer.Close()
}

// TestReadSemaphoreNotSet tests normal operation when no read semaphore is configured.
func TestReadSemaphoreNotSet(t *testing.T) {
	// Create keys.
	clientKey, _ := btcec.NewPrivateKey()
	serverKey, _ := btcec.NewPrivateKey()

	// Create mock connections.
	clientConn, _, err := createMockBrontidePairWithKeys(clientKey, serverKey)
	require.NoError(t, err)

	// Create peer WITHOUT read semaphore.
	cfg := SimplePeerConfig{
		KeyGenerator: NewStaticKeyGenerator(&keychain.PrivKeyECDH{PrivKey: clientKey}),
		Target: NodeAddress{
			PubKey:  serverKey.PubKey(),
			Address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		},
		Features: DefaultTestFeatures(),
		Timeouts: fn.Some(DefaultTimeouts()),
		// No ReadSemaphore set.
	}

	peer, err := NewSimplePeer(cfg)
	require.NoError(t, err)

	// Set the connection and mark as connected.
	peer.conn = clientConn
	peer.state.Store(int32(StateConnected))

	// Queue messages.
	for i := 0; i < 3; i++ {
		testMsg := &lnwire.UpdateAddHTLC{ID: uint64(i)}
		clientConn.QueueIncomingMessage(testMsg)
	}

	// Track received messages.
	messageCount := 0
	done := make(chan struct{})
	go func() {
		for range peer.ReceiveMessages() {
			messageCount++
			if messageCount == 3 {
				close(done)
			}
		}
	}()

	// Start the read handler.
	peer.wg.Add(1)
	go peer.readHandler()

	// Should receive all messages immediately without needing tokens.
	select {
	case <-done:
		// Success - got all messages.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for messages without semaphore")
	}

	// Verify we got all 3 messages.
	require.Equal(t, 3, messageCount)

	// Clean up.
	peer.Close()
}