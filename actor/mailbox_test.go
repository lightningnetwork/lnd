package actor

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMessage is a test message type that embeds BaseMessage.
type TestMessage struct {
	BaseMessage
	Value int
}

// MessageType returns the type name of the message for routing/filtering.
func (tm TestMessage) MessageType() string {
	return "TestMessage"
}

// TestChannelMailboxSend tests the Send method of ChannelMailbox.
func TestChannelMailboxSend(t *testing.T) {
	t.Run("successful send", func(t *testing.T) {
		mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)
		ctx := context.Background()
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}

		sent := mailbox.Send(ctx, env)
		require.True(t, sent, "Send should succeed")
	})

	t.Run("send with cancelled context", func(t *testing.T) {
		mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 1)
		// Fill the mailbox first.
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}
		mailbox.TrySend(env)

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel immediately.
		cancel()

		env2 := envelope[TestMessage, int]{
			message: TestMessage{Value: 43},
			promise: nil,
		}

		sent := mailbox.Send(ctx, env2)
		require.False(t, sent, "Send should fail with cancelled context")
	})

	t.Run("send to closed mailbox", func(t *testing.T) {
		mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)
		mailbox.Close()

		ctx := context.Background()
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}

		sent := mailbox.Send(ctx, env)
		require.False(t, sent, "Send should fail on closed mailbox")
	})
}

// TestChannelMailboxTrySend tests the TrySend method of ChannelMailbox.
func TestChannelMailboxTrySend(t *testing.T) {
	t.Run("successful try send", func(t *testing.T) {
		mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}

		sent := mailbox.TrySend(env)
		require.True(t, sent, "TrySend should succeed")
	})

	t.Run("try send to full mailbox", func(t *testing.T) {
		mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 1)
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}

		// Fill the mailbox.
		sent := mailbox.TrySend(env)
		require.True(t, sent, "First TrySend should succeed")

		// Try to send again - should fail.
		sent = mailbox.TrySend(env)
		require.False(t, sent, "TrySend should fail on full mailbox")
	})

	t.Run("try send to closed mailbox", func(t *testing.T) {
		mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)
		mailbox.Close()

		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}

		sent := mailbox.TrySend(env)
		require.False(t, sent, "TrySend should fail on closed mailbox")
	})
}

// TestChannelMailboxReceive tests the Receive method of ChannelMailbox.
func TestChannelMailboxReceive(t *testing.T) {
	t.Run("receive messages", func(t *testing.T) {
		mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)
		ctx := context.Background()

		// Send some messages.
		for i := 0; i < 3; i++ {
			env := envelope[TestMessage, int]{
				message: TestMessage{Value: i},
				promise: nil,
			}
			mailbox.Send(ctx, env)
		}

		// Start receiving in a goroutine.
		var received []int
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for env := range mailbox.Receive(ctx) {
				received = append(received, env.message.Value)
			}
		}()

		// Close the mailbox after sending all messages.
		mailbox.Close()
		wg.Wait()

		require.Len(t, received, 3, "Should receive 3 messages")
		require.Equal(t, []int{0, 1, 2}, received, "Should receive messages in order")
	})

	t.Run("receive with cancelled context", func(t *testing.T) {
		mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)
		ctx, cancel := context.WithCancel(context.Background())

		// Send a message.
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}
		mailbox.Send(context.Background(), env)

		// Start receiving.
		var received int
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for env := range mailbox.Receive(ctx) {
				received++
				_ = env
			}
		}()

		// Cancel the context.
		cancel()
		wg.Wait()

		// Might receive 0 or 1 message depending on timing.
		require.LessOrEqual(t, received, 1,
			"Should stop receiving after context cancel")
	})
}

// TestChannelMailboxClose tests the Close and IsClosed methods.
func TestChannelMailboxClose(t *testing.T) {
	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)

	require.False(t, mailbox.IsClosed(), "Mailbox should not be closed initially")

	mailbox.Close()
	require.True(t, mailbox.IsClosed(), "Mailbox should be closed after Close()")

	// Closing again should be safe.
	mailbox.Close()
	require.True(t, mailbox.IsClosed(), "Mailbox should remain closed")
}

// TestChannelMailboxDrain tests the Drain method of ChannelMailbox.
func TestChannelMailboxDrain(t *testing.T) {
	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)
	ctx := context.Background()

	// Send some messages.
	for i := 0; i < 3; i++ {
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: i},
			promise: nil,
		}
		mailbox.Send(ctx, env)
	}

	// Close the mailbox.
	mailbox.Close()

	// Drain messages.
	var drained []int
	for env := range mailbox.Drain() {
		drained = append(drained, env.message.Value)
	}

	require.Len(t, drained, 3, "Should drain 3 messages")
	require.Equal(t, []int{0, 1, 2}, drained, "Should drain messages in order")
}

// TestChannelMailboxConcurrent tests concurrent operations on ChannelMailbox.
func TestChannelMailboxConcurrent(t *testing.T) {
	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 100)
	ctx := context.Background()

	const numSenders = 10
	const messagesPerSender = 100

	var wg sync.WaitGroup

	// Start multiple senders.
	for i := 0; i < numSenders; i++ {
		wg.Add(1)
		go func(senderID int) {
			defer wg.Done()
			for j := 0; j < messagesPerSender; j++ {
				env := envelope[TestMessage, int]{
					message: TestMessage{Value: senderID*1000 + j},
					promise: nil,
				}
				mailbox.Send(ctx, env)
			}
		}(i)
	}

	// Start receiver.
	received := make([]int, 0, numSenders*messagesPerSender)
	var receiverWg sync.WaitGroup
	receiverWg.Add(1)
	go func() {
		defer receiverWg.Done()
		for env := range mailbox.Receive(ctx) {
			received = append(received, env.message.Value)
		}
	}()

	// Wait for all senders to complete.
	wg.Wait()

	// Close the mailbox now that all sends are complete.
	mailbox.Close()
	receiverWg.Wait()

	require.Len(t, received, numSenders*messagesPerSender,
		"Should receive all messages")
}

// TestChannelMailboxZeroCapacity tests that zero capacity defaults to 1.
func TestChannelMailboxZeroCapacity(t *testing.T) {
	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 0)

	// Should default to capacity of 1.
	env := envelope[TestMessage, int]{
		message: TestMessage{Value: 42},
		promise: nil,
	}

	sent := mailbox.TrySend(env)
	require.True(t, sent, "Should be able to send one message")

	// Second send should fail (mailbox full).
	sent = mailbox.TrySend(env)
	require.False(t, sent, "Second send should fail on full mailbox")
}

// TestChannelMailboxActorContext tests that the mailbox respects the actor's
// context for cancellation.
func TestChannelMailboxActorContext(t *testing.T) {
	t.Run("send respects actor context", func(t *testing.T) {
		actorCtx, actorCancel := context.WithCancel(context.Background())
		mailbox := NewChannelMailbox[TestMessage, int](actorCtx, 1)

		// Fill the mailbox.
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}
		mailbox.TrySend(env)

		// Cancel the actor context.
		actorCancel()

		// Try to send with a fresh caller context - should fail due to
		// actor context cancellation.
		callerCtx := context.Background()
		env2 := envelope[TestMessage, int]{
			message: TestMessage{Value: 43},
			promise: nil,
		}

		sent := mailbox.Send(callerCtx, env2)
		require.False(t, sent, "Send should fail when actor context is cancelled")
	})

	t.Run("receive respects actor context", func(t *testing.T) {
		actorCtx, actorCancel := context.WithCancel(context.Background())
		mailbox := NewChannelMailbox[TestMessage, int](actorCtx, 10)

		// Send a message.
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: 42},
			promise: nil,
		}
		mailbox.Send(context.Background(), env)

		// Start receiving with a fresh context.
		callerCtx := context.Background()
		var received int
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for env := range mailbox.Receive(callerCtx) {
				received++
				_ = env
			}
		}()

		// Cancel the actor context.
		actorCancel()
		wg.Wait()

		// Should have stopped receiving due to actor context cancellation.
		require.LessOrEqual(t, received, 1,
			"Should stop receiving when actor context is cancelled")
	})
}

// TestMailboxConcurrentSendAndClose tests concurrent Send and Close operations
// to ensure no race conditions or panics occur.
func TestMailboxConcurrentSendAndClose(t *testing.T) {
	const numSenders = 20
	const sendsPerSender = 100

	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 100)
	ctx := context.Background()

	var wg sync.WaitGroup

	// Start receiver to drain messages.
	var recvWg sync.WaitGroup
	recvWg.Add(1)
	go func() {
		defer recvWg.Done()
		for range mailbox.Receive(ctx) {
			// Just drain.
		}
	}()

	// Start multiple senders.
	for i := 0; i < numSenders; i++ {
		wg.Add(1)
		go func(senderID int) {
			defer wg.Done()
			for j := 0; j < sendsPerSender; j++ {
				env := envelope[TestMessage, int]{
					message: TestMessage{Value: senderID*1000 + j},
					promise: nil,
				}
				// Send may fail if mailbox closes, that's ok.
				mailbox.Send(ctx, env)
			}
		}(i)
	}

	// Concurrently close the mailbox multiple times from different
	// goroutines.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mailbox.Close()
		}()
	}

	wg.Wait()
	recvWg.Wait()

	// Mailbox should be closed.
	require.True(t, mailbox.IsClosed(), "Mailbox should be closed")

	// Further sends should fail without panic.
	env := envelope[TestMessage, int]{
		message: TestMessage{Value: 999},
		promise: nil,
	}
	sent := mailbox.Send(ctx, env)
	require.False(t, sent, "Send should fail on closed mailbox")
}

// TestMailboxConcurrentTrySendAndClose tests concurrent TrySend and Close
// operations to ensure no race conditions or panics occur.
func TestMailboxConcurrentTrySendAndClose(t *testing.T) {
	const numSenders = 20
	const sendsPerSender = 100

	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)

	var wg sync.WaitGroup

	// Start multiple senders using TrySend.
	for i := 0; i < numSenders; i++ {
		wg.Add(1)
		go func(senderID int) {
			defer wg.Done()
			for j := 0; j < sendsPerSender; j++ {
				env := envelope[TestMessage, int]{
					message: TestMessage{Value: senderID*1000 + j},
					promise: nil,
				}
				// TrySend may fail if mailbox is full or closed.
				mailbox.TrySend(env)
			}
		}(i)
	}

	// Concurrently close the mailbox.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mailbox.Close()
		}()
	}

	wg.Wait()

	// Mailbox should be closed.
	require.True(t, mailbox.IsClosed(), "Mailbox should be closed")

	// Further sends should fail without panic.
	env := envelope[TestMessage, int]{
		message: TestMessage{Value: 999},
		promise: nil,
	}
	sent := mailbox.TrySend(env)
	require.False(t, sent, "TrySend should fail on closed mailbox")
}

// TestMailboxMultipleCloseCallers tests that multiple goroutines calling
// Close() simultaneously don't cause panics or issues.
func TestMailboxMultipleCloseCallers(t *testing.T) {
	const numClosers = 100

	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 10)

	var wg sync.WaitGroup

	// Start many goroutines all trying to close the mailbox.
	for i := 0; i < numClosers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mailbox.Close()
		}()
	}

	wg.Wait()

	// Mailbox should be closed exactly once.
	require.True(t, mailbox.IsClosed(), "Mailbox should be closed")

	// Calling Close again should be safe.
	mailbox.Close()
	require.True(t, mailbox.IsClosed(), "Mailbox should remain closed")
}

// TestMailboxCloseWhileSending tests closing the mailbox while multiple
// senders are actively sending messages.
func TestMailboxCloseWhileSending(t *testing.T) {
	const numSenders = 10
	const sendsPerSender = 1000

	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 100)
	ctx := context.Background()

	var sendWg sync.WaitGroup

	// Start multiple senders.
	for i := 0; i < numSenders; i++ {
		sendWg.Add(1)
		go func(senderID int) {
			defer sendWg.Done()
			for j := 0; j < sendsPerSender; j++ {
				env := envelope[TestMessage, int]{
					message: TestMessage{Value: senderID*1000 + j},
					promise: nil,
				}
				// Send may fail after close, that's expected.
				mailbox.Send(ctx, env)
			}
		}(i)
	}

	// Start receiver to drain messages.
	var recvWg sync.WaitGroup
	recvWg.Add(1)
	receivedCount := 0
	go func() {
		defer recvWg.Done()
		for range mailbox.Receive(ctx) {
			receivedCount++
		}
	}()

	// Close mailbox while sends are happening.
	mailbox.Close()

	sendWg.Wait()
	recvWg.Wait()

	// Should have received at least some messages (exact count depends on
	// timing).
	t.Logf("Received %d messages before close", receivedCount)

	// Mailbox should be closed.
	require.True(t, mailbox.IsClosed(), "Mailbox should be closed")
}

// TestMailboxStressTest performs a high-concurrency stress test with multiple
// senders, receivers, and close operations.
func TestMailboxStressTest(t *testing.T) {
	const numSenders = 50
	const numReceivers = 5
	const sendsPerSender = 200

	mailbox := NewChannelMailbox[TestMessage, int](context.Background(), 200)
	ctx := context.Background()

	var sendWg sync.WaitGroup

	// Start multiple senders.
	for i := 0; i < numSenders; i++ {
		sendWg.Add(1)
		go func(senderID int) {
			defer sendWg.Done()
			for j := 0; j < sendsPerSender; j++ {
				env := envelope[TestMessage, int]{
					message: TestMessage{Value: senderID*1000 + j},
					promise: nil,
				}
				mailbox.Send(ctx, env)
			}
		}(i)
	}

	// Start multiple receivers.
	var recvWg sync.WaitGroup
	for i := 0; i < numReceivers; i++ {
		recvWg.Add(1)
		go func() {
			defer recvWg.Done()
			for range mailbox.Receive(ctx) {
				// Just drain messages.
			}
		}()
	}

	// Wait for all sends to complete.
	sendWg.Wait()

	// Close mailbox.
	mailbox.Close()

	// Wait for all receivers to finish.
	recvWg.Wait()

	// Mailbox should be closed.
	require.True(t, mailbox.IsClosed(), "Mailbox should be closed")
}
