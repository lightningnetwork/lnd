package queue

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// queueMachine is the generic state machine logic for testing
// BackpressureQueue. T must be comparable for use in assertions.
type queueMachine[T comparable] struct {
	tb rapid.TB

	capacity int

	queue *BackpressureQueue[T]

	modelQueue []T

	dropPredicate DropPredicate[T]

	itemGenerator *rapid.Generator[T]
}

// Enqueue is a state machine action. It enqueues an item and updates the model.
func (m *queueMachine[T]) Enqueue(t *rapid.T) {
	item := m.itemGenerator.Draw(t, "item")

	err := m.queue.Enqueue(context.Background(), item)

	actualDrop := false
	if errors.Is(err, ErrQueueFullAndDropped) {
		actualDrop = true
	} else if err != nil {
		// If Enqueue with background context returns an error other
		// than ErrQueueFullAndDropped, it's unexpected.
		m.tb.Fatalf(
			"Enqueue with background context returned "+
				"unexpected error: %v", err,
		)
	}

	if !actualDrop {
		// If the item was not dropped, it must have been enqueued. Add
		// it to the model. The modelQueue should not exceed capacity.
		// This is also checked in Check().
		m.modelQueue = append(m.modelQueue, item)
	}
}

// Dequeue is a state machine action. It dequeues an item and updates the model.
func (m *queueMachine[T]) Dequeue(t *rapid.T) {
	if len(m.modelQueue) == 0 {
		// If the model is empty, the actual queue channel should also
		// be empty.
		require.Zero(
			m.tb, len(m.queue.ch),
			"actual queue channel not empty when model is empty",
		)

		// Attempting to dequeue from an empty queue should block. We
		// verify this by trying to dequeue with a very short timeout.
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Millisecond,
		)
		defer cancel()

		result := m.queue.Dequeue(ctx)
		require.True(
			m.tb, result.IsErr(),
			"dequeue should return error on empty queue with "+
				"timeout",
		)
		require.ErrorIs(
			m.tb, result.Err(), context.DeadlineExceeded,
			"dequeue should block on empty queue",
		)

		return
	}

	// The model is not empty, so we expect to dequeue an item.
	expectedItem := m.modelQueue[0]
	m.modelQueue = m.modelQueue[1:]

	// Perform the dequeue operation, this should succeed.
	result := m.queue.Dequeue(context.Background())
	actualItem, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(
		m.tb, expectedItem, actualItem,
		"dequeued item does not match model (FIFO violation or "+
			"model error)",
	)
}

// Check is called by rapid after each action to verify invariants.
func (m *queueMachine[T]) Check(t *rapid.T) {
	// Invariant 1: The length of the internal channel must not exceed
	// capacity.
	require.LessOrEqual(
		m.tb, len(m.queue.ch), m.capacity,
		"queue channel length exceeds capacity",
	)

	// Invariant 2: The length of our model queue must match the length of
	// the actual queue's channel.
	require.Equal(
		m.tb, len(m.modelQueue), len(m.queue.ch),
		"model queue length mismatch with actual queue channel "+
			"length",
	)
}

// intQueueMachine is a concrete wrapper for queueMachine[int] for rapid.
type intQueueMachine struct {
	*queueMachine[int]
}

// NewIntqueueMachine creates a new queueMachine specialized for int items.
func NewIntqueueMachine(rt *rapid.T) *intQueueMachine {
	// Draw from the rapid distribution for the made params of our
	// queue.
	capacity := rapid.IntRange(1, 50).Draw(rt, "capacity")
	minThreshold := rapid.IntRange(0, capacity).Draw(rt, "minThreshold")
	maxThreshold := rapid.IntRange(
		minThreshold, capacity,
	).Draw(rt, "maxThreshold")

	// Draw a seed for this machine's local RNG using rapid. This makes
	// the predicate's randomness part of rapid's generated test case.
	machineSeed := rapid.Int64().Draw(rt, "machine_rng_seed")
	localRngFixed := rand.New(rand.NewSource(machineSeed))

	rt.Logf(
		"NewIntqueueMachine: capacity=%d, minT=%d, maxT=%d, "+
			"machineSeed=%d",
		capacity, minThreshold, maxThreshold, machineSeed,
	)

	predicate := RandomEarlyDrop[int](
		minThreshold, maxThreshold,
		WithRandSource(localRngFixed.Float64),
	)

	q := NewBackpressureQueue(capacity, predicate)

	return &intQueueMachine{
		queueMachine: &queueMachine[int]{
			tb:            rt,
			capacity:      capacity,
			queue:         q,
			modelQueue:    make([]int, 0, capacity),
			dropPredicate: predicate,
			itemGenerator: rapid.IntRange(-1000, 1000),
		},
	}
}

// Enqueue forwards the call to the generic queueMachine.
func (m *intQueueMachine) Enqueue(t *rapid.T) { m.queueMachine.Enqueue(t) }

// Dequeue forwards the call to the generic queueMachine.
func (m *intQueueMachine) Dequeue(t *rapid.T) { m.queueMachine.Dequeue(t) }

// Check forwards the call to the generic queueMachine.
func (m *intQueueMachine) Check(t *rapid.T) { m.queueMachine.Check(t) }

// TestBackpressureQueueRapidInt is the main property-based test for
// BackpressureQueue using the IntqueueMachine state machine.
func TestBackpressureQueueRapidInt(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Initialize the state machine instance within the
		// property function. NewIntqueueMachine expects *rapid.T,
		// which rt is.
		machine := NewIntqueueMachine(rt)

		// Generate the actions map from the machine's methods.
		// Rapid will randomly call the methods, and then use the
		// `Check` method to verify invariants.
		rt.Repeat(rapid.StateMachineActions(machine))
	})
}

// TestBackpressureQueueEnqueueCancellation tests that Enqueue respects
// context cancellation when it would otherwise block.
func TestBackpressureQueueEnqueueCancellation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		capacity := rapid.IntRange(1, 20).Draw(rt, "capacity")

		// Use a predicate that never drops when full, to force blocking
		// behavior.
		q := NewBackpressureQueue(
			capacity,
			func(_ int, _ int) bool { return false },
		)

		// Fill the queue to its capacity. The predicate always returns
		// false, so no drops expected.
		for i := 0; i < capacity; i++ {
			err := q.Enqueue(context.Background(), i)
			require.NoError(
				rt, err, "enqueue failed during setup: %v", err,
			)
		}
		require.Equal(
			rt, capacity, len(q.ch),
			"queue should be full after setup",
		)

		// Attempt to enqueue one more item with an immediately cancelled
		// context.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := q.Enqueue(ctx, 999)
		require.Error(
			rt, err,
			"enqueue should have returned an error for cancelled "+
				"context",
		)
		require.ErrorIs(
			rt, err, context.Canceled,
			"error should be context.Canceled",
		)

		// Ensure the queue state (length) is unchanged.
		require.Equal(
			rt, capacity, len(q.ch),
			"queue length changed after cancelled enqueue attempt",
		)
	})
}

// TestBackpressureQueueDequeueCancellation tests that Dequeue respects
// context cancellation when the queue is empty and it would otherwise block.
func TestBackpressureQueueDequeueCancellation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		capacity := rapid.IntRange(0, 20).Draw(rt, "capacity")

		// The predicate doesn't matter much here as the queue will be
		// empty.
		q := NewBackpressureQueue(
			capacity, RandomEarlyDrop[int](0, capacity),
		)

		require.Zero(
			rt, len(q.ch),
			"queue should be empty initially for Dequeue "+
				"cancellation test",
		)

		// Attempt to dequeue from the empty queue with an immediately
		// cancelled context.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		result := q.Dequeue(ctx)
		require.True(
			rt, result.IsErr(),
			"dequeue should have returned an error for cancelled "+
				"context",
		)
		require.ErrorIs(
			rt, result.Err(), context.Canceled,
			"error should be context.Canceled",
		)
	})
}

// TestBackpressureQueueComposedPredicate demonstrates testing with a
// composed predicate. This is a scenario-based test rather than a full
// property-based state machine.
func TestBackpressureQueueComposedPredicate(t *testing.T) {
	capacity := 10
	minThresh, maxThresh := 3, 7

	// Use a deterministic random source for this specific test case
	// to ensure predictable behavior of RandomEarlyDrop.
	const testSeed = int64(12345)
	localRng := rand.New(rand.NewSource(testSeed))

	redPred := RandomEarlyDrop[int](
		minThresh, maxThresh, WithRandSource(localRng.Float64),
	)

	// Next, we'll define a custom predicate: drop items with value 42.
	customValuePredicate := func(_ int, item int) bool {
		return item == 42
	}

	// We'll also make a composed predicate: drop if RED says so OR if
	// item is 42.
	composedPredicate := func(queueLen int, item int) bool {
		isRedDrop := redPred(queueLen, item)
		isCustomDrop := customValuePredicate(queueLen, item)
		return isRedDrop || isCustomDrop
	}

	q := NewBackpressureQueue(capacity, composedPredicate)

	// Scenario 1: Enqueue item 42 when queue length is between min/max
	// thresholds. As we're below the max threshold, we shouldn't drop
	// anything.
	for i := 0; i < minThresh; i++ {
		// All items aren't 42, and queue is not full enough for RED
		// to drop.
		err := q.Enqueue(context.Background(), i)
		require.NoErrorf(
			t, err,
			"enqueue S1 setup item %d (qLen before: %d) should "+
				"not be dropped. Predicate was "+
				"redPred(%d,%d) || customPred(%d,%d)",
			i, len(q.ch)-1, len(q.ch)-1, i, len(q.ch)-1, i,
		)

	}

	currentLen := len(q.ch)
	require.Equal(t, minThresh, currentLen, "queue length after S1 setup")

	// Enqueue item 42. customValuePredicate is true, so
	// composedPredicate is true. Item 42 should be dropped regardless
	// of what redPred decides.
	err := q.Enqueue(context.Background(), 42)
	require.ErrorIs(
		t, err, ErrQueueFullAndDropped,
		"item 42 should have been dropped by composed predicate",
	)
	require.Equal(
		t, currentLen, len(q.ch),
		"queue length should not change after dropping 42",
	)

	// Re-create the main SUT queue with the composedPredicate. We will
	// manually fill its channel to capacity to bypass Enqueue logic
	// for setup.
	q = NewBackpressureQueue(capacity, composedPredicate)
	for i := 0; i < capacity; i++ {
		q.ch <- i
	}
	require.Equal(
		t, capacity, len(q.ch),
		"queue manually filled to capacity for S2 test",
	)

	err = q.Enqueue(context.Background(), 100)

	// Expect drop because queue is full (len=capacity), so
	// redPred(capacity, 100) is true. customValuePredicate(capacity,
	// 100) is false. Thus, composedPredicate should be true.
	require.ErrorIs(
		t, err, ErrQueueFullAndDropped,
		"item 100 should be dropped (due to RED part of composed "+
			"predicate) when queue full",
	)
	require.Equal(
		t, capacity, len(q.ch),
		"queue length should not change after dropping 100",
	)
}
