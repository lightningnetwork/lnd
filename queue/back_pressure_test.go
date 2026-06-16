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
	if errors.Is(err, ErrItemDropped) {
		actualDrop = true
	} else if err != nil {
		// If Enqueue with background context returns an error other than
		// ErrItemDropped, it's unexpected.
		m.tb.Fatalf("Enqueue with background context returned "+
			"unexpected error: %v", err)
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
			m.tb, len(m.queue.ch), "actual queue channel not "+
				"empty when model is empty",
		)

		// Attempting to dequeue from an empty queue should block. We
		// verify this by trying to dequeue with a very short timeout.
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Millisecond,
		)
		defer cancel()

		result := m.queue.Dequeue(ctx)
		require.True(
			m.tb, result.IsErr(), "dequeue "+
				"should return error on empty queue with timeout",
		)
		require.ErrorIs(
			m.tb, result.Err(),
			context.DeadlineExceeded, "dequeue should "+
				"block on empty queue",
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
		m.tb, expectedItem, actualItem, "dequeued item does not "+
			"match model (FIFO violation or model error)",
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
		"model queue length mismatch with actual queue channel length",
	)
}

// intQueueMachine is a concrete wrapper for queueMachine[int] for rapid.
type intQueueMachine struct {
	*queueMachine[int]
}

// NewIntQueueMachine creates a new queueMachine specialized for int items.
func NewIntQueueMachine(rt *rapid.T) *intQueueMachine {
	// Draw from the rapid distribution for the made params of our queue.
	capacity := rapid.IntRange(1, 50).Draw(rt, "capacity")
	minThreshold := rapid.IntRange(
		0, capacity-1,
	).Draw(rt, "minThreshold")
	maxThreshold := rapid.IntRange(
		minThreshold+1, capacity,
	).Draw(rt, "maxThreshold")

	// Draw a seed for this machine's local RNG using rapid. This makes the
	// predicate's randomness part of rapid's generated test case.
	machineSeed := rapid.Int64().Draw(rt, "machine_rng_seed")
	localRngFixed := rand.New(rand.NewSource(machineSeed))

	rt.Logf("NewIntQueueMachine: capacity=%d, minT=%d, maxT=%d, "+
		"machineSeed=%d", capacity, minThreshold, maxThreshold,
		machineSeed)

	redCheck, err := RandomEarlyDrop(
		minThreshold, maxThreshold,
		WithRandSource(localRngFixed.Float64),
	)
	require.NoError(rt, err)
	predicate := AsDropPredicate[int](redCheck)

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
// BackpressureQueue using the IntQueueMachine state machine.
func TestBackpressureQueueRapidInt(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Initialize the state machine instance within the property
		// function. NewIntQueueMachine expects *rapid.T, which rt is.
		machine := NewIntQueueMachine(rt)

		// Generate the actions map from the machine's methods. Rapid
		// will randomly call the methods, and then use the `Check`
		// method to verify invariants.
		rt.Repeat(rapid.StateMachineActions(machine))
	})
}

// TestBackpressureQueueEnqueueCancellation tests that Enqueue respects context
// cancellation when it would otherwise block.
func TestBackpressureQueueEnqueueCancellation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		capacity := rapid.IntRange(1, 20).Draw(rt, "capacity")

		// Use a predicate that never drops when full, to force blocking
		// behavior.
		q := NewBackpressureQueue(capacity,
			func(_ int, _ int) bool { return false },
		)

		// Fill the queue to its capacity. The predicate always returns
		// false, so no drops expected.
		for i := range capacity {
			err := q.Enqueue(context.Background(), i)
			require.NoError(
				rt, err, "enqueue failed during setup: %v", err,
			)
		}
		require.Equal(
			rt, capacity, len(q.ch), "queue "+
				"should be full after setup",
		)

		// Attempt to enqueue one more item with an immediately cancelled
		// context.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := q.Enqueue(ctx, 999)
		require.Error(
			rt, err, "enqueue should have "+
				"returned an error for cancelled context",
		)
		require.ErrorIs(
			rt, err, context.Canceled,
			"error should be context.Canceled",
		)

		// Ensure the queue state (length) is unchanged.
		require.Equal(
			rt, capacity, len(q.ch), "queue length changed "+
				"after cancelled enqueue attempt",
		)
	})
}

// TestBackpressureQueueDequeueCancellation tests that Dequeue respects context
// cancellation when the queue is empty and it would otherwise block.
func TestBackpressureQueueDequeueCancellation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		capacity := rapid.IntRange(1, 20).Draw(rt, "capacity")

		// The predicate doesn't matter much here as the queue will be
		// empty. Use a never-drop predicate for simplicity.
		q := NewBackpressureQueue(capacity,
			func(_ int, _ int) bool { return false },
		)

		require.Zero(
			rt, len(q.ch), "queue should be empty initially for "+
				"Dequeue cancellation test",
		)

		// Attempt to dequeue from the empty queue with an immediately
		// cancelled context.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		result := q.Dequeue(ctx)
		require.ErrorIs(
			rt, result.UnwrapRightOr(nil),
			context.Canceled,
			"error should be context.Canceled",
		)
	})
}

// TestBackpressureQueueComposedPredicate demonstrates testing with a composed
// predicate. This is a scenario-based test rather than a full property-based
// state machine.
func TestBackpressureQueueComposedPredicate(t *testing.T) {
	capacity := 10
	minThresh, maxThresh := 3, 7

	// Use a deterministic random source for this specific test case to
	// ensure predictable behavior of RandomEarlyDrop.
	const testSeed = int64(12345)
	localRng := rand.New(rand.NewSource(testSeed))

	redCheck, err := RandomEarlyDrop(
		minThresh, maxThresh, WithRandSource(localRng.Float64),
	)
	require.NoError(t, err)

	// Next, we'll define a custom predicate: drop items with value 42.
	customValuePredicate := func(_ int, item int) bool {
		return item == 42
	}

	// We'll also make a composed predicate: drop if RED says so OR if item
	// is 42.
	composedPredicate := func(queueLen int, item int) bool {
		isRedDrop := redCheck(queueLen)
		isCustomDrop := customValuePredicate(queueLen, item)
		return isRedDrop || isCustomDrop
	}

	q := NewBackpressureQueue(capacity, composedPredicate)

	// Scenario 1: Enqueue item 42 when queue length is between min/max
	// thresholds. As we're below the max threshold, we shouldn't drop
	// anything.
	for i := range minThresh {
		// All items aren't 42, and queue is not full enough for RED to
		// drop.
		err := q.Enqueue(context.Background(), i)
		require.NoErrorf(t, err, "enqueue S1 setup "+
			"item %d (qLen before: %d) should not be dropped. "+
			"Predicate was redCheck(%d) || customPred(%d,%d)",
			i, len(q.ch)-1, len(q.ch)-1, len(q.ch)-1, i)

	}

	currentLen := len(q.ch)
	require.Equal(t, minThresh, currentLen, "queue length after S1 setup")

	// Enqueue item 42. customValuePredicate is true, so composedPredicate
	// is true. Item 42 should be dropped regardless of what redCheck
	// decides.
	err = q.Enqueue(context.Background(), 42)
	require.ErrorIs(
		t, err, ErrItemDropped,
		"item 42 should have been dropped by composed predicate",
	)
	require.Equal(
		t, currentLen, len(q.ch), "queue length should not change "+
			"after dropping 42",
	)

	// Re-create the main SUT queue with the composedPredicate. We will
	// manually fill its channel to capacity to bypass Enqueue logic for
	// setup.
	q = NewBackpressureQueue(capacity, composedPredicate)
	for i := range capacity {
		q.ch <- i
	}
	require.Equal(
		t, capacity, len(q.ch), "queue manually filled to capacity "+
			"for S2 test",
	)

	err = q.Enqueue(context.Background(), 100)

	// Expect drop because queue is full (len=capacity), so
	// redCheck(capacity) is true. customValuePredicate(capacity, 100)
	// is false. Thus, composedPredicate should be true.
	require.ErrorIs(
		t, err, ErrItemDropped,
		"item 100 should be dropped (due to RED part "+
			"of composed predicate) when queue full",
	)
	require.Equal(
		t, capacity, len(q.ch), "queue length should not change "+
			"after dropping 100",
	)
}

// TestBackpressureQueueTryEnqueue verifies non-blocking enqueue with drop
// predicate checks.
func TestBackpressureQueueTryEnqueue(t *testing.T) {
	t.Parallel()

	const capacity = 5
	const dropThreshold = 3

	alwaysDropAboveThreshold := DropPredicate[int](
		func(queueLen int, _ int) bool {
			return queueLen >= dropThreshold
		},
	)

	q := NewBackpressureQueue(capacity, alwaysDropAboveThreshold)

	// Fill up to the drop threshold — all should succeed.
	for i := range dropThreshold {
		ok := q.TryEnqueue(i)
		require.True(t, ok, "TryEnqueue(%d) should succeed", i)
	}

	require.Equal(t, dropThreshold, q.Len())

	// Next TryEnqueue should be dropped by predicate.
	ok := q.TryEnqueue(99)
	require.False(t, ok, "should be dropped at threshold")
	require.Equal(t, dropThreshold, q.Len())

	// With a never-drop predicate, fill to capacity and verify TryEnqueue
	// returns false when the channel is full.
	q2 := NewBackpressureQueue(capacity,
		func(_ int, _ int) bool { return false },
	)
	for i := range capacity {
		ok := q2.TryEnqueue(i)
		require.True(t, ok, "TryEnqueue(%d) should succeed", i)
	}
	ok = q2.TryEnqueue(999)
	require.False(t, ok, "TryEnqueue should fail when channel is full")
}

// TestBackpressureQueueLenAndReceiveChan verifies Len and ReceiveChan.
func TestBackpressureQueueLenAndReceiveChan(t *testing.T) {
	t.Parallel()

	neverDrop := DropPredicate[int](func(_ int, _ int) bool {
		return false
	})
	q := NewBackpressureQueue(10, neverDrop)

	require.Equal(t, 0, q.Len())

	for i := range 3 {
		require.NoError(t, q.Enqueue(context.Background(), i))
	}
	require.Equal(t, 3, q.Len())

	// ReceiveChan should yield the items.
	ch := q.ReceiveChan()
	val := <-ch
	require.Equal(t, 0, val)
	require.Equal(t, 2, q.Len())
}

// TestBackpressureQueueClose verifies that Close shuts down the channel.
func TestBackpressureQueueClose(t *testing.T) {
	t.Parallel()

	neverDrop := DropPredicate[int](func(_ int, _ int) bool {
		return false
	})
	q := NewBackpressureQueue(10, neverDrop)

	for i := range 3 {
		require.NoError(t, q.Enqueue(context.Background(), i))
	}

	q.Close()

	// Remaining items should still be readable.
	var items []int
	for v := range q.ReceiveChan() {
		items = append(items, v)
	}
	require.Equal(t, []int{0, 1, 2}, items)
}

// TestBackpressureQueueDoubleClose verifies that calling Close twice does not
// panic.
func TestBackpressureQueueDoubleClose(t *testing.T) {
	t.Parallel()

	neverDrop := DropPredicate[int](func(_ int, _ int) bool {
		return false
	})
	q := NewBackpressureQueue(5, neverDrop)

	q.Close()
	q.Close() // must not panic
}

// TestBackpressureQueueEnqueueAfterClose verifies that Enqueue returns
// ErrQueueClosed after the queue has been closed.
func TestBackpressureQueueEnqueueAfterClose(t *testing.T) {
	t.Parallel()

	neverDrop := DropPredicate[int](func(_ int, _ int) bool {
		return false
	})
	q := NewBackpressureQueue(5, neverDrop)

	require.NoError(t, q.Enqueue(context.Background(), 1))
	q.Close()

	err := q.Enqueue(context.Background(), 2)
	require.ErrorIs(t, err, ErrQueueClosed)
}

// TestBackpressureQueueTryEnqueueAfterClose verifies that TryEnqueue returns
// false after the queue has been closed.
func TestBackpressureQueueTryEnqueueAfterClose(t *testing.T) {
	t.Parallel()

	neverDrop := DropPredicate[int](func(_ int, _ int) bool {
		return false
	})
	q := NewBackpressureQueue(5, neverDrop)

	q.Close()

	ok := q.TryEnqueue(1)
	require.False(t, ok, "TryEnqueue after Close should return false")
}
