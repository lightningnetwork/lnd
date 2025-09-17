package lnp2p

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestPeerActorWithHarness tests PeerActor using the test harness.
func TestPeerActorWithHarness(t *testing.T) {
	t.Run("MessageDistribution", testMessageDistribution)
	t.Run("SendMessage", testSendMessage)
	t.Run("ConnectionLifecycle", testConnectionLifecycle)
	t.Run("ErrorHandling", testErrorHandling)
	t.Run("ConcurrentOperations", testConcurrentOperations)
	t.Run("MessageProcessing", testMessageProcessing)
	t.Run("AutoConnect", testAutoConnect)
	t.Run("ServiceKeyManagement", testServiceKeyManagement)
	t.Run("PingPongHandling", testPingPongHandling)
	t.Run("ErrorWarningHandling", testErrorWarningHandling)
	t.Run("DistributeMessageRefresh", testDistributeMessageRefresh)
}

func testMessageDistribution(t *testing.T) {
	// Create harness with connected mock.
	harness := newTestHarness(t, ConnectedMockConfig()).
		withActorSystem()
	defer harness.cleanupAll()

	// Create message counters.
	key1, count1 := harness.createMessageCounter("handler1")
	key2, count2 := harness.createMessageCounter("handler2")

	// Create peer actor with service keys.
	peerActor := harness.createPeerActor(key1, key2)
	harness.startPeerActor(peerActor)
	defer harness.stopPeerActor(peerActor)

	// Simulate message distribution.
	testMsg := &MessageReceived{
		Message:    lnwire.NewPing(100),
		From:       harness.mock.RemotePubKey(),
		ReceivedAt: time.Now(),
	}
	ctx := context.Background()
	peerActor.distributeMessage(ctx, testMsg)

	// Wait for processing.
	time.Sleep(100 * time.Millisecond)

	// Both handlers should have received the message.
	require.Equal(t, int32(2), *count1+*count2)
}

func testSendMessage(t *testing.T) {
	// Create harness with connected mock.
	harness := newTestHarness(t, ConnectedMockConfig()).
		withActorSystem()
	defer harness.cleanupAll()

	// Set up send expectation.
	testMsg := lnwire.NewPing(100)
	harness.expectSendMessage(testMsg)

	// Create and test peer actor.
	peerActor := harness.createPeerActor()
	err := harness.sendMessage(peerActor, testMsg)
	require.NoError(t, err)

	harness.assertExpectations()
}

func testConnectionLifecycle(t *testing.T) {
	ctx := context.Background()

	// Create harness that starts disconnected.
	harness := newTestHarness(t, DefaultMockConfig()).
		withActorSystem()
	defer harness.cleanupAll()

	// Set up connect expectation and simulate successful connection.
	// After 2 calls, return connected.
	harness.expectConnect(ctx).
		simulateConnectionStateChange(2, true)

	peerActor := harness.createPeerActor()

	// Initially disconnected.
	harness.requireActorDisconnected(peerActor)

	// Request connection.
	result := peerActor.Receive(ctx, &ConnectRequest{Timeout: time.Second})
	resp, err := result.Unpack()
	require.NoError(t, err)
	connResp, ok := resp.(*ConnectResponse)
	require.True(t, ok)
	require.True(t, connResp.Success)

	// Test WaitForConnection when already connected.
	waitResult := peerActor.Receive(ctx, &WaitForConnectionRequest{})
	waitResp, err := waitResult.Unpack()
	require.NoError(t, err)
	waitConnResp, ok := waitResp.(*WaitForConnectionResponse)
	require.True(t, ok)
	require.True(t, waitConnResp.AlreadyConnected)
}

func testErrorHandling(t *testing.T) {
	ctx := context.Background()

	t.Run("ConnectionError", func(t *testing.T) {
		connError := fmt.Errorf("connection failed")
		harness := newTestHarness(t, &MockConnectionConfig{
			IsConnected:  false,
			ConnectError: connError,
			RemotePubKey: newTestPubKey(),
			LocalPubKey:  newTestPubKey(),
		}).withActorSystem()
		defer harness.cleanupAll()

		harness.expectConnectWithError(ctx, connError)

		peerActor := harness.createPeerActor()
		result := peerActor.Receive(ctx, &ConnectRequest{Timeout: time.Second})

		// The request should be accepted (Success: true).
		connResp := harness.requireConnectRequestAccepted(result)

		// But the actual connection should fail via the Future.
		harness.requireConnectionFailed(connResp.Future)
	})

	t.Run("SendError", func(t *testing.T) {
		sendError := fmt.Errorf("send failed")
		harness := newTestHarness(t, &MockConnectionConfig{
			IsConnected:      true,
			SendMessageError: sendError,
			RemotePubKey:     newTestPubKey(),
			LocalPubKey:      newTestPubKey(),
		}).withActorSystem()
		defer harness.cleanupAll()

		testMsg := lnwire.NewPing(100)
		harness.mock.On("SendMessage", testMsg).Return(sendError).Once()

		peerActor := harness.createPeerActor()
		err := harness.sendMessage(peerActor, testMsg)
		require.Error(t, err)
	})
}

func testConcurrentOperations(t *testing.T) {
	harness := newTestHarness(t, ConnectedMockConfig()).
		withActorSystem()
	defer harness.cleanupAll()

	peerActor := harness.createPeerActor()

	var wg sync.WaitGroup
	errors := make(chan error, 10)

	// Concurrent status requests.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			result := peerActor.Receive(ctx, &GetStatusRequest{})
			resp, err := result.Unpack()
			if err != nil {
				errors <- err
				return
			}
			if _, ok := resp.(*StatusResponse); !ok {
				errors <- fmt.Errorf("unexpected response type")
			}
		}()
	}

	// Concurrent service key operations.
	testKey := actor.NewServiceKey[PeerMessage, PeerResponse]("test-concurrent")

	wg.Add(1)
	go func() {
		defer wg.Done()
		harness.addServiceKey(peerActor, testKey)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		harness.requireServiceKeyCount(peerActor, 1)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		harness.removeServiceKey(peerActor, testKey)
	}()

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("Concurrent operation error: %v", err)
	}
}

func testMessageProcessing(t *testing.T) {
	ctx := context.Background()
	// Use UpdateAddHTLC messages which will be distributed to service keys.
	messages := []lnwire.Message{
		&lnwire.UpdateAddHTLC{ID: 1},
		&lnwire.UpdateAddHTLC{ID: 2},
		&lnwire.UpdateAddHTLC{ID: 3},
	}

	// Start disconnected and let AutoConnect establish the connection.
	harness := newTestHarness(t, &MockConnectionConfig{
		IsConnected:  false,
		RemotePubKey: newTestPubKey(),
		LocalPubKey:  newTestPubKey(),
		Messages:     messages,
		RemoteAddr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		LocalAddr:    &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345},
	}).withActorSystem()
	defer harness.cleanupAll()

	// Create message counter.
	messageKey, receivedCount := harness.createMessageCounter("processor")

	// Set up connect expectation and simulate successful connection.
	// After 2 calls, return connected.
	harness.expectConnect(ctx).
		simulateConnectionStateChange(2, true)

	// Create actor with AutoConnect to trigger connection and message processing.
	cfg := PeerActorConfig{
		Connection:   harness.mock,
		Receptionist: harness.system.Receptionist(),
		MessageSinks: []*MessageSink{{ServiceKey: messageKey}},
		// This will trigger connection and processMessages.
		AutoConnect:  true,
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	result := peerActor.Start(ctx)
	startResp, err := result.Unpack()
	require.NoError(t, err)
	require.True(t, startResp.Success)
	defer harness.stopPeerActor(peerActor)

	// Wait for messages to be processed.
	time.Sleep(200 * time.Millisecond)

	// All messages should have been received.
	require.Equal(t, int32(len(messages)), atomic.LoadInt32(receivedCount))
}

func testAutoConnect(t *testing.T) {
	ctx := context.Background()

	harness := newTestHarness(t, DefaultMockConfig()).
		withActorSystem()
	defer harness.cleanupAll()

	// Set up auto-connect expectations.
	harness.expectConnect(ctx)
	harness.mock.On("IsConnected").Return(true).Maybe()

	// Create actor with AutoConnect enabled.
	cfg := PeerActorConfig{
		Connection:   harness.mock,
		Receptionist: harness.system.Receptionist(),
		MessageSinks: []*MessageSink{},
		AutoConnect:  true,
	}

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Start should trigger auto-connect.
	harness.startPeerActor(peerActor)
	defer harness.stopPeerActor(peerActor)

	harness.assertExpectations()
}

func testServiceKeyManagement(t *testing.T) {
	harness := newTestHarness(t, ConnectedMockConfig()).
		withActorSystem()
	defer harness.cleanupAll()

	peerActor := harness.createPeerActor()

	// Initially no keys.
	harness.requireServiceKeyCount(peerActor, 0)

	// Add keys.
	key1 := harness.createServiceKey("key1", func(ctx context.Context, msg PeerMessage) fn.Result[PeerResponse] {
		return fn.Ok[PeerResponse](nil)
	})
	key2 := harness.createServiceKey("key2", func(ctx context.Context, msg PeerMessage) fn.Result[PeerResponse] {
		return fn.Ok[PeerResponse](nil)
	})

	harness.addServiceKey(peerActor, key1)
	harness.requireServiceKeyCount(peerActor, 1)

	harness.addServiceKey(peerActor, key2)
	harness.requireServiceKeyCount(peerActor, 2)

	// Try to add duplicate - should indicate already exists.
	ctx := context.Background()
	addResp := peerActor.Receive(ctx, &AddServiceKeyRequest{
		MessageSink: &MessageSink{
			ServiceKey: key1,
		},
	})
	addResult, err := addResp.Unpack()
	require.NoError(t, err)
	addResponse := addResult.(*AddServiceKeyResponse)
	require.False(t, addResponse.Success)
	require.True(t, addResponse.AlreadyExists)
	// Still 2 unique keys (key1 and key2).
	harness.requireServiceKeyCount(peerActor, 2)

	// Remove keys.
	harness.removeServiceKey(peerActor, key1)
	harness.requireServiceKeyCount(peerActor, 1)

	harness.removeServiceKey(peerActor, key2)
	harness.requireServiceKeyCount(peerActor, 0)

	// Try to remove non-existent - should fail.
	removed := peerActor.RemoveServiceKey(key1)
	require.False(t, removed)
}

func testPingPongHandling(t *testing.T) {
	ctx := context.Background()

	// Create test harness with messages to process.
	harness := newTestHarness(t, &MockConnectionConfig{
		IsConnected:  true,
		RemotePubKey: newTestPubKey(),
		LocalPubKey:  newTestPubKey(),
		Messages: []lnwire.Message{
			// Should trigger pong response.
			lnwire.NewPing(10),
			// Should be handled internally.
			lnwire.NewPong([]byte("test")),
			// Should be distributed.
			&lnwire.Init{
				GlobalFeatures: lnwire.NewRawFeatureVector(),
				Features:       lnwire.NewRawFeatureVector(),
			},
		},
		RemoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		LocalAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345},
	})
	defer harness.cleanupAll()

	// Track non-ping/pong messages.
	var messageCount atomic.Int32

	// Create service key that should NOT receive ping/pong.
	messageKey := harness.createServiceKey("message-handler", func(ctx context.Context, msg PeerMessage) fn.Result[PeerResponse] {
		if msgRcvd, ok := msg.(*MessageReceived); ok {
			// Should not receive ping or pong.
			switch msgRcvd.Message.(type) {
			case *lnwire.Ping, *lnwire.Pong:
				t.Errorf("Received ping/pong message that should be handled internally")
			default:
				messageCount.Add(1)
			}
		}
		return fn.Ok[PeerResponse](nil)
	})

	// Expect a pong response to the ping.
	harness.expectPongResponse(10)

	// Create and start peer actor.
	cfg := harness.createPeerActorConfig(&MessageSink{ServiceKey: messageKey})
	cfg.AutoConnect = true

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Start the actor to trigger processMessages.
	result := peerActor.Start(ctx)
	startResp, err := result.Unpack()
	require.NoError(t, err)
	require.True(t, startResp.Success)

	// Wait for processing.
	time.Sleep(200 * time.Millisecond)

	// Verify only the Init message was distributed.
	require.Equal(t, int32(1), messageCount.Load())

	// Stop the actor.
	err = peerActor.Stop()
	require.NoError(t, err)
}

func testErrorWarningHandling(t *testing.T) {
	ctx := context.Background()

	// Create test harness with Error and Warning messages.
	harness := newTestHarness(t, &MockConnectionConfig{
		IsConnected:  true,
		RemotePubKey: newTestPubKey(),
		LocalPubKey:  newTestPubKey(),
		Messages: []lnwire.Message{
			&lnwire.Error{
				ChanID: lnwire.ChannelID{1, 2, 3},
				Data:   []byte("test error"),
			},
			&lnwire.Warning{
				ChanID: lnwire.ChannelID{4, 5, 6},
				Data:   []byte("test warning"),
			},
		},
		RemoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		LocalAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345},
	})
	defer harness.cleanupAll()

	// Track messages and errors/warnings.
	var messageCount, errorCount, warningCount atomic.Int32

	// Create service key to receive all messages.
	messageKey := harness.createServiceKey("handler", func(ctx context.Context, msg PeerMessage) fn.Result[PeerResponse] {
		switch m := msg.(type) {
		case *MessageReceived:
			messageCount.Add(1)
			// Verify we get the original Error/Warning messages.
			switch m.Message.(type) {
			case *lnwire.Error:
				t.Log("Received Error message")
			case *lnwire.Warning:
				t.Log("Received Warning message")
			}
		case *PeerError:
			errorCount.Add(1)
			require.Contains(t, m.Error.Error(), "remote error: test error")
			require.Equal(t, ErrorTypeRemote, m.ErrorType)
		case *PeerWarning:
			warningCount.Add(1)
			require.Equal(t, "test warning", m.Warning)
		}
		return fn.Ok[PeerResponse](nil)
	})

	// Create and start peer actor.
	cfg := harness.createPeerActorConfig(&MessageSink{ServiceKey: messageKey})
	cfg.AutoConnect = true

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Start the actor.
	result := peerActor.Start(ctx)
	startResp, err := result.Unpack()
	require.NoError(t, err)
	require.True(t, startResp.Success)

	// Wait for processing.
	time.Sleep(200 * time.Millisecond)

	// Verify both original messages and special notifications were received.
	// Error and Warning as MessageReceived.
	require.Equal(t, int32(2), messageCount.Load())
	// PeerError notification.
	require.Equal(t, int32(1), errorCount.Load())
	// PeerWarning notification.
	require.Equal(t, int32(1), warningCount.Load())

	// Stop the actor.
	err = peerActor.Stop()
	require.NoError(t, err)
}

func testDistributeMessageRefresh(t *testing.T) {
	ctx := context.Background()

	// Create test harness.
	harness := newTestHarness(t, nil)
	defer harness.cleanupAll()

	var messageCount atomic.Int32

	// Create service key WITHOUT registering it yet.
	messageKey := actor.NewServiceKey[PeerMessage, PeerResponse]("late-handler")

	// Configure peer actor BEFORE registering the actor.
	cfg := harness.createPeerActorConfig(&MessageSink{ServiceKey: messageKey})

	peerActor, err := NewPeerActor(cfg)
	require.NoError(t, err)

	// Start the actor.
	harness.startPeerActor(peerActor)

	// Verify the sink has no actorRef yet.
	require.Nil(t, peerActor.messageSinks[0].actorRef)

	// Try to distribute a message - should attempt refresh but find nothing.
	testMsg := &MessageReceived{
		Message:    lnwire.NewPing(0),
		From:       harness.getMock().RemotePubKey(),
		ReceivedAt: time.Now(),
	}
	peerActor.distributeMessage(ctx, testMsg)

	// Now register the actor (late registration).
	harness.registerServiceKey(messageKey, "late-handler", func(ctx context.Context, msg PeerMessage) fn.Result[PeerResponse] {
		messageCount.Add(1)
		return fn.Ok[PeerResponse](nil)
	})

	// Distribute another message - should refresh and find the actor.
	peerActor.distributeMessage(ctx, testMsg)

	// Wait for processing.
	time.Sleep(100 * time.Millisecond)

	// Verify the message was received after late registration.
	require.Equal(t, int32(1), messageCount.Load())

	// Verify the actorRef was set during distribution.
	require.NotNil(t, peerActor.messageSinks[0].actorRef)

	// Stop the actor.
	err = peerActor.Stop()
	require.NoError(t, err)
}

