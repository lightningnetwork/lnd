package queue

import (
	"context"
	"errors"
	"math/rand"

	"github.com/lightningnetwork/lnd/fn/v2"
)

var (
	// ErrItemDropped is returned by Enqueue when the item is dropped due
	// to the DropPredicate.
	ErrItemDropped = errors.New("item dropped by drop predicate")

	// ErrQueueClosed is returned by Dequeue when the internal channel has
	// been closed.
	ErrQueueClosed = errors.New("queue closed")

	// ErrInvalidCapacity is returned by NewBackpressureQueue when capacity
	// is not positive.
	ErrInvalidCapacity = errors.New(
		"backpressure queue capacity must be greater than zero",
	)

	// ErrNilPredicate is returned by NewBackpressureQueue when the
	// predicate is nil.
	ErrNilPredicate = errors.New(
		"backpressure queue predicate must not be nil",
	)

	// ErrNegativeMinThreshold is returned by RandomEarlyDrop when
	// minThreshold is negative.
	ErrNegativeMinThreshold = errors.New(
		"RED minThreshold must be >= 0",
	)

	// ErrInvalidThresholdOrder is returned by RandomEarlyDrop when
	// maxThreshold is not strictly greater than minThreshold.
	ErrInvalidThresholdOrder = errors.New(
		"RED maxThreshold must be > minThreshold",
	)
)

// DropCheckFunc decides whether to drop an item based solely on the current
// queue depth. This is the natural return type for length-only strategies such
// as RandomEarlyDrop.
type DropCheckFunc func(queueLen int) bool

// DropPredicate decides whether to drop an item based on the current queue
// length and the item itself. It returns true to drop, false to enqueue. Use
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

// BackpressureQueue is a generic, fixed-capacity queue with predicate-based
// drop behavior. It uses the DropPredicate to perform early drops (e.g.,
// RED-style) before the queue is completely full.
type BackpressureQueue[T any] struct {
	ch            chan T
	dropPredicate DropPredicate[T]
}

// NewBackpressureQueue creates a new BackpressureQueue with the given capacity
// and drop predicate. Returns an error if capacity is not positive or predicate
// is nil.
func NewBackpressureQueue[T any](capacity int,
	predicate DropPredicate[T]) (*BackpressureQueue[T], error) {

	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}
	if predicate == nil {
		return nil, ErrNilPredicate
	}

	return &BackpressureQueue[T]{
		ch:            make(chan T, capacity),
		dropPredicate: predicate,
	}, nil
}

// Enqueue attempts to add an item to the queue, respecting context
// cancellation. Returns ErrItemDropped if dropped, or context error if ctx is
// done before enqueue. Otherwise, `nil` is returned on success.
func (q *BackpressureQueue[T]) Enqueue(ctx context.Context,
	item T) error {

	// First, consult the drop predicate based on the current queue length.
	// If the predicate decides to drop the item, return an error.
	//
	// NOTE: The queue length snapshot used here can become stale before the
	// actual channel send below. This is acceptable because RED is
	// inherently probabilistic and approximate — a slightly outdated length
	// does not compromise correctness.
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
// context done. Returns the item, an error if ctx is done before an item is
// available, or ErrQueueClosed if the channel has been closed.
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

// Close closes the internal channel. After Close, no more items can be
// enqueued. Remaining items can still be received via ReceiveChan.
func (q *BackpressureQueue[T]) Close() {
	close(q.ch)
}

// redConfig holds configuration for RandomEarlyDrop.
type redConfig struct {
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
// Detection (RED), inspired by TCP-RED queue management. Returns an error if
// the threshold parameters are invalid.
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

		// In between the thresholds, linearly scale the drop
		// probability. At this point, minThreshold <= queueLen <
		// maxThreshold, which implies minThreshold < maxThreshold, so
		// the denominator is guaranteed to be positive.
		denominator := float64(maxThreshold - minThreshold)
		p := float64(queueLen-minThreshold) / denominator

		return cfg.randSrc() < p
	}

	return dropFn, nil
}
