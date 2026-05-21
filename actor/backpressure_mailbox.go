package actor

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/queue"
)

// BackpressureMailboxCfg holds optional configuration for a
// BackpressureMailbox. A zero-value config is valid and disables
// automatic first-drop logging.
type BackpressureMailboxCfg struct {
	// Name is a human-readable label included in the first-drop log
	// line. When empty, no automatic logging is performed and the
	// caller is expected to use FirstDropClaim() to drive its own
	// logging.
	Name string
}

// BackpressureMailbox implements the Mailbox interface using a
// queue.BackpressureQueue as its core buffer. The BackpressureQueue's drop
// predicate is consulted on every Send/TrySend, allowing RED-style load
// shedding before the mailbox is full.
//
// Every predicate rejection is counted and exposed via Dropped(). Two
// independent one-shot flags exist for first-drop signaling:
//
//   - When a Name is configured, the mailbox emits a single info-level
//     log the first time the predicate fires (gated by firstLog).
//   - FirstDropClaim() exposes a separate one-shot flag (firstDrop)
//     for callers that want to emit their own log at the call site.
//
// The two flags are independent: using one does not consume the other.
type BackpressureMailbox[M Message, R any] struct {
	// queue is the underlying backpressure-aware buffer.
	queue *queue.BackpressureQueue[envelope[M, R]]

	// closed tracks whether the mailbox has been closed.
	closed atomic.Bool

	// dropped counts the total number of messages rejected by the
	// drop predicate since the mailbox was created.
	dropped atomic.Uint64

	// firstLog is consumed internally by the counting predicate
	// wrapper. When Name is set, the wrapper CAS-flips this flag
	// on the first rejection and emits an info-level log line.
	firstLog atomic.Bool

	// firstDrop is exposed to callers via FirstDropClaim(). It is
	// independent of firstLog so that the internal auto-log and an
	// external caller-driven log can coexist without racing for
	// the same flag.
	firstDrop atomic.Bool

	// name is the optional label from BackpressureMailboxCfg.Name.
	name string

	// mu protects Send/TrySend operations to prevent
	// send-on-closed-channel panics. Close() acquires write lock,
	// Send/TrySend acquire read lock.
	mu sync.RWMutex

	// closeOnce ensures Close() executes exactly once.
	closeOnce sync.Once

	// actorCtx is the actor's context for lifecycle management.
	actorCtx context.Context
}

// NewBackpressureMailbox creates a new mailbox backed by a
// BackpressureQueue. The shouldDrop function is called with the current
// queue depth on every send attempt; if it returns true the message is
// dropped and the internal drop counter is incremented.
func NewBackpressureMailbox[M Message, R any](
	actorCtx context.Context,
	capacity int,
	shouldDrop queue.DropCheckFunc,
	cfg BackpressureMailboxCfg,
) *BackpressureMailbox[M, R] {

	if capacity <= 0 {
		capacity = 1
	}

	mb := &BackpressureMailbox[M, R]{
		actorCtx: actorCtx,
		name:     cfg.Name,
	}

	// Wrap the caller's predicate so every rejection increments
	// the drop counter and, when a name is configured, emits a
	// one-shot info log on the first drop.
	inner := queue.AsDropPredicate[envelope[M, R]](shouldDrop)
	counting := func(queueLen int, item envelope[M, R]) bool {
		if !inner(queueLen, item) {
			return false
		}

		mb.dropped.Add(1)

		if mb.name != "" &&
			mb.firstLog.CompareAndSwap(false, true) {

			log.Infof("Mailbox(%s): first message "+
				"dropped (queue_depth=%d)",
				mb.name, queueLen)
		}

		return true
	}

	mb.queue = queue.NewBackpressureQueue(capacity, counting)

	return mb
}

// Dropped returns the total number of messages rejected by the drop
// predicate since the mailbox was created.
func (m *BackpressureMailbox[M, R]) Dropped() uint64 {
	return m.dropped.Load()
}

// FirstDropClaim atomically returns true exactly once, and only after
// at least one message has actually been dropped by the predicate. It
// is intended for call sites that want to emit a one-shot log or
// metric when the mailbox first starts shedding load. This flag is
// independent of the built-in first-drop log gated by
// BackpressureMailboxCfg.Name; using one does not consume the other.
func (m *BackpressureMailbox[M, R]) FirstDropClaim() bool {
	if m.dropped.Load() == 0 {
		return false
	}

	return m.firstDrop.CompareAndSwap(false, true)
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
