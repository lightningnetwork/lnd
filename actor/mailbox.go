package actor

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
)

// Mailbox represents the message queue for an actor. It provides methods for
// sending messages and receiving them via an iterator pattern.
type Mailbox[M Message, R any] interface {
	// Send attempts to send an envelope to the mailbox with context-based
	// cancellation. Returns true if sent successfully, false if the
	// context was cancelled or the mailbox is closed.
	Send(ctx context.Context, env envelope[M, R]) bool

	// TrySend attempts to send without blocking. Returns true if the
	// envelope was sent, false if the mailbox is full or closed.
	TrySend(env envelope[M, R]) bool

	// Receive returns an iterator for consuming messages from the mailbox.
	// The iterator will yield messages until the mailbox is closed or the
	// context is cancelled.
	Receive(ctx context.Context) iter.Seq[envelope[M, R]]

	// Close closes the mailbox, preventing new messages from being sent.
	// Any remaining messages can still be consumed via Receive.
	Close()

	// IsClosed returns true if the mailbox has been closed.
	IsClosed() bool

	// Drain returns an iterator that yields all remaining messages in the
	// mailbox after it has been closed. This is useful for cleanup.
	Drain() iter.Seq[envelope[M, R]]
}

// ChannelMailbox is a channel-based implementation of the Mailbox interface.
type ChannelMailbox[M Message, R any] struct {
	ch     chan envelope[M, R]
	closed atomic.Bool

	// mu protects Send/TrySend operations to prevent send-on-closed-channel
	// panics. Close() acquires write lock, Send/TrySend acquire read lock.
	mu sync.RWMutex

	// closeOnce ensures Close() executes exactly once.
	closeOnce sync.Once

	// actorCtx is the actor's context for lifecycle management.
	actorCtx context.Context
}

// NewChannelMailbox creates a new channel-based mailbox with the specified
// buffer capacity and actor context.
func NewChannelMailbox[M Message, R any](actorCtx context.Context,
	capacity int) *ChannelMailbox[M, R] {

	if capacity <= 0 {
		capacity = 1
	}
	return &ChannelMailbox[M, R]{
		ch:       make(chan envelope[M, R], capacity),
		actorCtx: actorCtx,
	}
}

// Send implements Mailbox.Send with context-aware blocking send.
func (m *ChannelMailbox[M, R]) Send(ctx context.Context,
	env envelope[M, R]) bool {

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.IsClosed() {
		return false
	}

	// Check if the send context is already cancelled before attempting
	// the select. This avoids the non-deterministic behavior when both
	// the channel send and context cancellation are ready simultaneously.
	if ctx.Err() != nil {
		return false
	}

	select {
	case m.ch <- env:
		return true
	case <-ctx.Done():
		return false
	case <-m.actorCtx.Done():
		// Actor is shutting down.
		return false
	}
}

// TrySend implements Mailbox.TrySend with non-blocking send.
func (m *ChannelMailbox[M, R]) TrySend(env envelope[M, R]) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.IsClosed() {
		return false
	}

	select {
	case m.ch <- env:
		return true
	default:
		return false
	}
}

// Receive implements Mailbox.Receive using iter.Seq pattern.
func (m *ChannelMailbox[M, R]) Receive(
	ctx context.Context) iter.Seq[envelope[M, R]] {
	return func(yield func(envelope[M, R]) bool) {
		for {
			select {
			case env, ok := <-m.ch:
				if !ok {
					return
				}

				if !yield(env) {
					return
				}

			case <-ctx.Done():
				return

			case <-m.actorCtx.Done():
				return
			}
		}
	}
}

// Close implements Mailbox.Close.
func (m *ChannelMailbox[M, R]) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.closed.Store(true)

		close(m.ch)
	})
}

// IsClosed implements Mailbox.IsClosed.
func (m *ChannelMailbox[M, R]) IsClosed() bool {
	return m.closed.Load()
}

// Drain implements Mailbox.Drain for cleanup after close.
func (m *ChannelMailbox[M, R]) Drain() iter.Seq[envelope[M, R]] {
	return func(yield func(envelope[M, R]) bool) {
		// Only drain if closed.
		if !m.IsClosed() {
			return
		}

		// Drain all remaining messages from the channel.
		for {
			select {
			case env, ok := <-m.ch:
				// Channel closed, nothing left to drain.
				if !ok {
					return
				}

				if !yield(env) {
					return
				}
			default:
				// Channel empty, done draining.
				return
			}
		}
	}
}
