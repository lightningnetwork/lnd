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
		m.tb.Fatalf(
			"Enqueue returned unexpected error: %v", err,
		)
	}

	if !actualDrop {
		m.modelQueue = append(m.modelQueue, item)
	}
}

// Dequeue is a state machine action. It dequeues an item and updates the model.
func (m *queueMachine[T]) Dequeue(t *rapid.T) {
	if len(m.modelQueue) == 0 {
		require.Zero(
			m.tb, len(m.queue.ch),
			"queue not empty when model is empty",
		)

		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Millisecond,
		)
		defer cancel()

		result := m.queue.Dequeue(ctx)
		require.ErrorIs(
			m.tb, result.Err(), context.DeadlineExceeded,
			"dequeue should block on empty queue",
		)

		return
	}

	expectedItem := m.modelQueue[0]
	m.modelQueue = m.modelQueue[1:]

	result := m.queue.Dequeue(context.Background())
	actualItem, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(
		m.tb, expectedItem, actualItem,
		"FIFO violation or model error",
	)
}

// Check is called by rapid after each action to verify invariants.
func (m *queueMachine[T]) Check(t *rapid.T) {
	require.LessOrEqual(
		m.tb, len(m.queue.ch), m.capacity,
		"queue length exceeds capacity",
	)
	require.Equal(
		m.tb, len(m.modelQueue), len(m.queue.ch),
		"model/actual length mismatch",
	)
}

// intQueueMachine is a concrete wrapper for queueMachine[int] for rapid.
type intQueueMachine struct {
	*queueMachine[int]
}

// NewIntqueueMachine creates a new queueMachine specialized for int items.
func NewIntqueueMachine(rt *rapid.T) *intQueueMachine {
	capacity := rapid.IntRange(1, 50).Draw(rt, "capacity")
	minThreshold := rapid.IntRange(0, capacity-1).Draw(
		rt, "minThreshold",
	)
	maxThreshold := rapid.IntRange(
		minThreshold+1, capacity,
	).Draw(rt, "maxThreshold")

	machineSeed := rapid.Int64().Draw(rt, "machine_rng_seed")
	localRngFixed := rand.New(rand.NewSource(machineSeed))

	rt.Logf(
		"NewIntqueueMachine: capacity=%d, minT=%d, maxT=%d, "+
			"seed=%d",
		capacity, minThreshold, maxThreshold, machineSeed,
	)

	redCheck, err := RandomEarlyDrop(
		minThreshold, maxThreshold,
		WithRandSource(localRngFixed.Float64),
	)
	require.NoError(rt, err)

	predicate := AsDropPredicate[int](redCheck)
	q, err := NewBackpressureQueue(capacity, predicate)
	require.NoError(rt, err)

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
		machine := NewIntqueueMachine(rt)
		rt.Repeat(rapid.StateMachineActions(machine))
	})
}

// TestBackpressureQueueCancellation tests that Enqueue and Dequeue respect
// context cancellation.
func TestBackpressureQueueCancellation(t *testing.T) {
	t.Run("enqueue", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			capacity := rapid.IntRange(1, 20).Draw(
				rt, "capacity",
			)

			q, err := NewBackpressureQueue(
				capacity,
				func(_ int, _ int) bool { return false },
			)
			require.NoError(rt, err)

			// Fill the queue to capacity.
			for i := range capacity {
				require.NoError(
					rt,
					q.Enqueue(context.Background(), i),
				)
			}

			// Enqueue with cancelled context should return
			// context.Canceled.
			ctx, cancel := context.WithCancel(
				context.Background(),
			)
			cancel()

			err = q.Enqueue(ctx, 999)
			require.ErrorIs(rt, err, context.Canceled)
			require.Equal(rt, capacity, len(q.ch))
		})
	})

	t.Run("dequeue", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			capacity := rapid.IntRange(1, 20).Draw(
				rt, "capacity",
			)

			redCheck, err := RandomEarlyDrop(0, capacity)
			require.NoError(rt, err)

			q, err := NewBackpressureQueue(
				capacity, AsDropPredicate[int](redCheck),
			)
			require.NoError(rt, err)

			// Dequeue with cancelled context from empty queue
			// should return context.Canceled.
			ctx, cancel := context.WithCancel(
				context.Background(),
			)
			cancel()

			result := q.Dequeue(ctx)
			require.ErrorIs(
				rt, result.Err(), context.Canceled,
			)
		})
	})
}

// TestBackpressureQueueComposedPredicate demonstrates testing with a composed
// predicate that combines RED with an item-specific drop rule.
func TestBackpressureQueueComposedPredicate(t *testing.T) {
	capacity := 10
	minThresh, maxThresh := 3, 7

	const testSeed = int64(12345)
	localRng := rand.New(rand.NewSource(testSeed))

	redCheck, err := RandomEarlyDrop(
		minThresh, maxThresh, WithRandSource(localRng.Float64),
	)
	require.NoError(t, err)

	// Composed predicate: drop if RED says so OR if item is 42.
	composedPredicate := func(queueLen int, item int) bool {
		return redCheck(queueLen) || item == 42
	}

	q, err := NewBackpressureQueue(capacity, composedPredicate)
	require.NoError(t, err)

	// Fill below minThresh — no drops expected.
	for i := range minThresh {
		require.NoError(t, q.Enqueue(context.Background(), i))
	}
	require.Equal(t, minThresh, len(q.ch))

	// Item 42 should always be dropped by the custom rule.
	err = q.Enqueue(context.Background(), 42)
	require.ErrorIs(t, err, ErrItemDropped)
	require.Equal(t, minThresh, len(q.ch))

	// Fill to capacity directly on the channel to test RED at full.
	q, err = NewBackpressureQueue(capacity, composedPredicate)
	require.NoError(t, err)
	for i := range capacity {
		q.ch <- i
	}

	// RED should drop at full capacity.
	err = q.Enqueue(context.Background(), 100)
	require.ErrorIs(t, err, ErrItemDropped)
	require.Equal(t, capacity, len(q.ch))
}

// TestBackpressureQueueTryEnqueue verifies non-blocking enqueue with drop
// predicate checks.
func TestBackpressureQueueTryEnqueue(t *testing.T) {
	t.Parallel()

	const capacity = 5
	const dropThreshold = 3

	q, err := NewBackpressureQueue(
		capacity,
		func(queueLen int, _ int) bool {
			return queueLen >= dropThreshold
		},
	)
	require.NoError(t, err)

	// Fill up to the drop threshold — all should succeed.
	for i := range dropThreshold {
		require.True(t, q.TryEnqueue(i))
	}
	require.Equal(t, dropThreshold, q.Len())

	// Next TryEnqueue should be dropped by predicate.
	require.False(t, q.TryEnqueue(99))
	require.Equal(t, dropThreshold, q.Len())

	// With a never-drop predicate, fill to capacity and verify TryEnqueue
	// returns false when the channel is full.
	q2, err := NewBackpressureQueue(
		capacity, func(_ int, _ int) bool { return false },
	)
	require.NoError(t, err)
	for i := range capacity {
		require.True(t, q2.TryEnqueue(i))
	}
	require.False(t, q2.TryEnqueue(999))
}

// TestBackpressureQueueHelpers tests Len, ReceiveChan, and Close.
func TestBackpressureQueueHelpers(t *testing.T) {
	t.Parallel()

	neverDrop := func(_ int, _ int) bool { return false }

	t.Run("len_and_receive_chan", func(t *testing.T) {
		t.Parallel()

		q, err := NewBackpressureQueue(10, neverDrop)
		require.NoError(t, err)

		require.Equal(t, 0, q.Len())

		for i := range 3 {
			require.NoError(
				t, q.Enqueue(context.Background(), i),
			)
		}
		require.Equal(t, 3, q.Len())

		// ReceiveChan should yield items in FIFO order.
		require.Equal(t, 0, <-q.ReceiveChan())
		require.Equal(t, 2, q.Len())
	})

	t.Run("close", func(t *testing.T) {
		t.Parallel()

		q, err := NewBackpressureQueue(10, neverDrop)
		require.NoError(t, err)

		for i := range 3 {
			require.NoError(
				t, q.Enqueue(context.Background(), i),
			)
		}

		q.Close()

		// Remaining items should still be readable via range.
		var items []int
		for v := range q.ReceiveChan() {
			items = append(items, v)
		}
		require.Equal(t, []int{0, 1, 2}, items)

		// After draining, Dequeue should return ErrQueueClosed.
		result := q.Dequeue(context.Background())
		require.ErrorIs(t, result.Err(), ErrQueueClosed)
	})
}

// TestBackpressureQueueValidation tests constructor and RED parameter
// validation using table-driven subtests.
func TestBackpressureQueueValidation(t *testing.T) {
	t.Parallel()

	t.Run("constructor", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name      string
			capacity  int
			predicate DropPredicate[int]
			wantErr   error
		}{
			{
				name:     "zero_capacity",
				capacity: 0,
				predicate: func(_ int, _ int) bool {
					return false
				},
				wantErr: ErrInvalidCapacity,
			},
			{
				name:     "negative_capacity",
				capacity: -1,
				predicate: func(_ int, _ int) bool {
					return false
				},
				wantErr: ErrInvalidCapacity,
			},
			{
				name:      "nil_predicate",
				capacity:  10,
				predicate: nil,
				wantErr:   ErrNilPredicate,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewBackpressureQueue(
					tc.capacity, tc.predicate,
				)
				require.ErrorIs(t, err, tc.wantErr)
			})
		}
	})

	t.Run("random_early_drop", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			min     int
			max     int
			wantErr error
		}{
			{
				name:    "negative_min",
				min:     -1,
				max:     10,
				wantErr: ErrNegativeMinThreshold,
			},
			{
				name:    "equal_thresholds",
				min:     5,
				max:     5,
				wantErr: ErrInvalidThresholdOrder,
			},
			{
				name:    "max_less_than_min",
				min:     10,
				max:     5,
				wantErr: ErrInvalidThresholdOrder,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := RandomEarlyDrop(tc.min, tc.max)
				require.ErrorIs(t, err, tc.wantErr)
			})
		}

		// Valid thresholds should succeed.
		dropFn, err := RandomEarlyDrop(0, 10)
		require.NoError(t, err)
		require.NotNil(t, dropFn)
	})
}
