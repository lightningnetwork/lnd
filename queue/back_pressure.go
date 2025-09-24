package queue

import (
	"context"
	"errors"
	"math/rand"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// DropPredicate decides whether to drop an item when the queue is full.
// It receives the current queue length and the item, and returns true to drop,
// false to enqueue.
type DropPredicate[T any] func(queueLen int, item T) bool

// ErrQueueFullAndDropped is returned by Enqueue when the item is dropped
// due to the DropPredicate.
var ErrQueueFullAndDropped = errors.New("queue full and item dropped")

// BackpressureQueue is a generic, fixed-capacity queue with predicate-based
// drop behavior. When full, it uses the DropPredicate to perform early drops
// (e.g., RED-style).
type BackpressureQueue[T any] struct {
	ch            chan T
	dropPredicate DropPredicate[T]
}

// NewBackpressureQueue creates a new BackpressureQueue with the given capacity
// and drop predicate.
func NewBackpressureQueue[T any](capacity int,
	predicate DropPredicate[T]) *BackpressureQueue[T] {

	return &BackpressureQueue[T]{
		ch:            make(chan T, capacity),
		dropPredicate: predicate,
	}
}

// Enqueue attempts to add an item to the queue, respecting context
// cancellation. Returns ErrQueueFullAndDropped if dropped, or context error if
// ctx is done before enqueue. Otherwise, `nil` is returned on success.
func (q *BackpressureQueue[T]) Enqueue(ctx context.Context,
	item T) error {

	// First, consult the drop predicate based on the current queue length.
	// If the predicate decides to drop the item, return true (dropped).
	if q.dropPredicate(len(q.ch), item) {
		return ErrQueueFullAndDropped
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

// Dequeue retrieves the next item from the queue, blocking until available or
// context done. Returns the item or an error if ctx is done before an item is
// available.
func (q *BackpressureQueue[T]) Dequeue(ctx context.Context) fn.Result[T] {
	select {

	case item := <-q.ch:
		return fn.Ok(item)

	case <-ctx.Done():
		return fn.Err[T](ctx.Err())
	}
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

// RandomEarlyDrop returns a DropPredicate that implements Random Early
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
func RandomEarlyDrop[T any](minThreshold, maxThreshold int, opts ...REDOption) DropPredicate[T] {
	cfg := redConfig{
		randSrc: rand.Float64,
	}

	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.randSrc == nil {
		cfg.randSrc = rand.Float64
	}

	return func(queueLen int, _ T) bool {
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
		// minThreshold <= queueLen < maxThreshold. This also implies
		// minThreshold < maxThreshold, so denominator won't be zero.
		denominator := float64(maxThreshold - minThreshold)

		p := float64(queueLen-minThreshold) / denominator

		return cfg.randSrc() < p
	}
}
