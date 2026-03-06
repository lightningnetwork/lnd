package actor

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/queue"
)

// BackpressureMailbox implements the Mailbox interface using a
// queue.BackpressureQueue as its core buffer. The BackpressureQueue's drop
// predicate is consulted on every Send/TrySend, allowing RED-style load
// shedding before the mailbox is full.
type BackpressureMailbox[M Message, R any] struct {
	// queue is the underlying backpressure-aware buffer.
	queue *queue.BackpressureQueue[envelope[M, R]]

	// closed tracks whether the mailbox has been closed.
	closed atomic.Bool

	// mu protects Send/TrySend operations to prevent send-on-closed-channel
	// panics. Close() acquires write lock, Send/TrySend acquire read lock.
	mu sync.RWMutex

	// closeOnce ensures Close() executes exactly once.
	closeOnce sync.Once

	// actorCtx is the actor's context for lifecycle management.
	actorCtx context.Context
}

// NewBackpressureMailbox creates a new mailbox backed by a BackpressureQueue.
// The shouldDrop function is called with the current queue depth on every send
// attempt; if it returns true the message is silently dropped.
func NewBackpressureMailbox[M Message, R any](
	actorCtx context.Context,
	capacity int,
	shouldDrop queue.DropCheckFunc,
) *BackpressureMailbox[M, R] {

	if capacity <= 0 {
		capacity = 1
	}

	pred := queue.AsDropPredicate[envelope[M, R]](shouldDrop)

	return &BackpressureMailbox[M, R]{
		queue:    queue.NewBackpressureQueue(capacity, pred),
		actorCtx: actorCtx,
	}
}

// Send attempts to send an envelope to the mailbox. The BackpressureQueue's
// drop predicate is consulted first; if it decides to drop, false is returned
// immediately. Otherwise the send blocks until the envelope is accepted, the
// caller's context is cancelled, or the actor's context is cancelled.
func (m *BackpressureMailbox[M, R]) Send(ctx context.Context,
	env envelope[M, R]) bool {

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.IsClosed() {
		return false
	}

	// Create a context that is cancelled when either the caller's context
	// or the actor's context is done, so that the blocking Enqueue
	// respects both.
	merged, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.actorCtx, cancel)
	defer stop()
	defer cancel()

	err := m.queue.Enqueue(merged, env)

	return err == nil
}

// TrySend attempts a non-blocking send. Returns false if the drop predicate
// rejects the message, the queue is at capacity, or the mailbox is closed.
func (m *BackpressureMailbox[M, R]) TrySend(env envelope[M, R]) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.IsClosed() {
		return false
	}

	return m.queue.TryEnqueue(env)
}

// Receive returns an iterator that yields envelopes from the mailbox until
// the mailbox is closed, the provided context is cancelled, or the actor's
// context is cancelled.
func (m *BackpressureMailbox[M, R]) Receive(
	ctx context.Context) iter.Seq[envelope[M, R]] {

	return func(yield func(envelope[M, R]) bool) {
		ch := m.queue.ReceiveChan()
		for {
			select {
			case env, ok := <-ch:
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

// Close closes the mailbox, preventing new messages from being sent. Any
// remaining messages can still be consumed via Drain.
func (m *BackpressureMailbox[M, R]) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.closed.Store(true)

		m.queue.Close()
	})
}

// IsClosed returns true if the mailbox has been closed.
func (m *BackpressureMailbox[M, R]) IsClosed() bool {
	return m.closed.Load()
}

// Drain returns an iterator that yields all remaining messages in the mailbox
// after it has been closed.
func (m *BackpressureMailbox[M, R]) Drain() iter.Seq[envelope[M, R]] {
	return func(yield func(envelope[M, R]) bool) {
		if !m.IsClosed() {
			return
		}

		ch := m.queue.ReceiveChan()
		for {
			select {
			case env, ok := <-ch:
				if !ok {
					return
				}

				if !yield(env) {
					return
				}
			default:
				return
			}
		}
	}
}
