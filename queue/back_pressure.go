package queue

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ErrQueueClosed is returned by Enqueue/TryEnqueue when the queue has already
// been closed.
var ErrQueueClosed = errors.New("queue closed")

// DropCheckFunc decides whether to drop an item based solely on the current
// queue depth. This is the natural return type for length-only strategies such
// as RandomEarlyDrop.
type DropCheckFunc func(queueLen int) bool

// DropPredicate decides whether to drop an item based on the current queue
// depth and the item itself. It returns true to drop, false to enqueue. Use
// this when the drop decision depends on the item itself; for length-only
// checks prefer DropCheckFunc.
type DropPredicate[T any] func(queueLen int, item T) bool

// AsDropPredicate adapts a length-only DropCheckFunc into a DropPredicate[T],
// ignoring the item.
func AsDropPredicate[T any](f DropCheckFunc) DropPredicate[T] {
	return func(queueLen int, _ T) bool {
		return f(queueLen)
	}
}

// ErrItemDropped is returned by Enqueue when the item is dropped by the
// DropPredicate. This can happen before the queue is actually full (e.g. with
// RED-style early drops).
var ErrItemDropped = errors.New("item dropped by drop predicate")

// ErrNegativeMinThreshold is returned by RandomEarlyDrop when minThreshold
// is negative.
var ErrNegativeMinThreshold = errors.New(
	"queue: minThreshold must be >= 0",
)

// ErrInvalidThresholdOrder is returned by RandomEarlyDrop when maxThreshold
// is not strictly greater than minThreshold.
var ErrInvalidThresholdOrder = errors.New(
	"queue: maxThreshold must be > minThreshold",
)

// BackpressureQueue is a generic, fixed-capacity queue with predicate-based
// drop behavior. When full, it uses the DropPredicate to perform early drops
// (e.g., RED-style).
type BackpressureQueue[T any] struct {
	ch            chan T
	dropPredicate DropPredicate[T]

	closed    atomic.Bool
	closeOnce sync.Once
}

// NewBackpressureQueue creates a new BackpressureQueue with the given capacity
// and drop predicate. Panics if capacity <= 0 or predicate is nil.
func NewBackpressureQueue[T any](capacity int,
	predicate DropPredicate[T]) *BackpressureQueue[T] {

	if capacity <= 0 {
		panic("queue: NewBackpressureQueue requires capacity > 0")
	}
	if predicate == nil {
		panic("queue: NewBackpressureQueue requires " +
			"a non-nil predicate")
	}

	return &BackpressureQueue[T]{
		ch:            make(chan T, capacity),
		dropPredicate: predicate,
	}
}

// Enqueue attempts to add an item to the queue, respecting context
// cancellation. Returns ErrItemDropped if dropped, or context error if ctx is
// done before enqueue. Otherwise, `nil` is returned on success.
func (q *BackpressureQueue[T]) Enqueue(ctx context.Context, item T) error {
	if q.closed.Load() {
		return ErrQueueClosed
	}

	// Consult the drop predicate based on the current queue length.
	//
	// NOTE: There is a TOCTOU gap here — the queue length snapshot can
	// become stale between this check and the channel send below if
	// there are multiple concurrent writers. This is acceptable because
	// RED is inherently probabilistic and approximate; a slightly
	// outdated length does not compromise correctness.
	if q.dropPredicate(len(q.ch), item) {
		return ErrItemDropped
	}

	// If the predicate decides not to drop, attempt to enqueue the item.
	select {
	case q.ch <- item:
		return nil

	default:
		// Channel is full, and the predicate decided not to drop. We
		// must block until space is available or context is cancelled.
		select {
		case q.ch <- item:
			return nil

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// TryEnqueue attempts to add an item to the queue without blocking. Returns
// true if successfully enqueued, false if the drop predicate rejected the item
// or the queue is at capacity.
func (q *BackpressureQueue[T]) TryEnqueue(item T) bool {
	if q.closed.Load() {
		return false
	}

	if q.dropPredicate(len(q.ch), item) {
		return false
	}

	select {
	case q.ch <- item:
		return true
	default:
		return false
	}
}

// Dequeue retrieves the next item from the queue, blocking until available or
// context done. Returns the item or an error if ctx is done before an item is
// available.
func (q *BackpressureQueue[T]) Dequeue(ctx context.Context) fn.Result[T] {
	select {

	case item, ok := <-q.ch:
		if !ok {
			return fn.Err[T](ErrQueueClosed)
		}
		return fn.Ok(item)

	case <-ctx.Done():
		return fn.Err[T](ctx.Err())
	}
}

// Len returns the current number of items buffered in the queue.
func (q *BackpressureQueue[T]) Len() int {
	return len(q.ch)
}

// ReceiveChan returns the receive-only end of the internal channel, allowing
// callers to select on it alongside other channels (e.g., context.Done).
func (q *BackpressureQueue[T]) ReceiveChan() <-chan T {
	return q.ch
}

// Close closes the internal channel. It is safe to call multiple times;
// only the first call has any effect. After Close, no more items can be
// enqueued. Remaining items can still be received via ReceiveChan.
func (q *BackpressureQueue[T]) Close() {
	q.closeOnce.Do(func() {
		q.closed.Store(true)
		close(q.ch)
	})
}

// redConfig holds configuration for RandomEarlyDrop.
type redConfig struct {
	// randSrc returns a float64 in [0.0, 1.0). It must be safe for
	// concurrent use if the returned DropCheckFunc will be called from
	// multiple goroutines. The default (math/rand.Float64) is safe since
	// Go 1.20+.
	randSrc func() float64
}

// REDOption is a functional option for configuring RandomEarlyDrop.
type REDOption func(*redConfig)

// WithRandSource provides a custom random number source (a function that
// returns a float64 between 0.0 and 1.0).
func WithRandSource(src func() float64) REDOption {
	return func(cfg *redConfig) {
		cfg.randSrc = src
	}
}

// RandomEarlyDrop returns a DropCheckFunc that implements Random Early
// Detection (RED), inspired by TCP-RED queue management.
//
// RED prevents sudden buffer overflows by proactively dropping packets before
// the queue is full. It establishes two thresholds:
//
//  1. minThreshold: queue length below which no drops occur.
//  2. maxThreshold: queue length at or above which all items are dropped.
//
// Between these points, the drop probability p increases linearly:
//
//	p = (queueLen - minThreshold) / (maxThreshold - minThreshold)
//
// For example, with minThreshold=15 and maxThreshold=35:
//   - At queueLen=15, p=0.0 (0% drop chance)
//   - At queueLen=25, p=0.5 (50% drop chance)
//   - At queueLen=35, p=1.0 (100% drop chance)
//
// This smooth ramp helps avoid tail-drop spikes, smooths queue occupancy,
// and gives early back-pressure signals to senders.
func RandomEarlyDrop(minThreshold, maxThreshold int,
	opts ...REDOption) (DropCheckFunc, error) {

	if minThreshold < 0 {
		return nil, ErrNegativeMinThreshold
	}
	if maxThreshold <= minThreshold {
		return nil, ErrInvalidThresholdOrder
	}

	cfg := redConfig{
		randSrc: rand.Float64,
	}

	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.randSrc == nil {
		cfg.randSrc = rand.Float64
	}

	// Precompute the denominator for the linear drop probability
	// scaling. Since minThreshold < maxThreshold is enforced above,
	// this is always positive.
	denominator := float64(maxThreshold - minThreshold)

	dropFn := func(queueLen int) bool {
		// If the queue is below the minimum threshold, then we never
		// drop.
		if queueLen < minThreshold {
			return false
		}

		// If the queue is at or above the maximum threshold, then we
		// always drop.
		if queueLen >= maxThreshold {
			return true
		}

		// If we're in the middle, then we implement linear scaling of
		// the drop probability based on our thresholds. At this point,
		// minThreshold <= queueLen < maxThreshold.
		p := float64(queueLen-minThreshold) / denominator

		return cfg.randSrc() < p
	}

	return dropFn, nil
}
