package queue

import (
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewCircularBuffer tests the size parameter check when creating a circular
// buffer.
func TestNewCircularBuffer(t *testing.T) {
	tests := []struct {
		name          string
		size          int
		expectedError error
	}{
		{
			name:          "zero size",
			size:          0,
			expectedError: errInvalidSize,
		},
		{
			name:          "negative size",
			size:          -1,
			expectedError: errInvalidSize,
		},
		{
			name:          "ok size",
			size:          1,
			expectedError: nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			_, err := NewCircularBuffer(test.size)
			if err != test.expectedError {
				t.Fatalf("expected: %v, got: %v",
					test.expectedError, err)
			}
		})
	}
}

// TestCircularBuffer tests the adding and listing of items in a circular
// buffer.
func TestCircularBuffer(t *testing.T) {
	tests := []struct {
		name          string
		size          int
		itemCount     int
		expectedItems []interface{}
	}{
		{
			name:          "no elements",
			size:          5,
			itemCount:     0,
			expectedItems: nil,
		},
		{
			name:      "single element",
			size:      5,
			itemCount: 1,
			expectedItems: []interface{}{
				0,
			},
		},
		{
			name:      "no wrap, not full",
			size:      5,
			itemCount: 4,
			expectedItems: []interface{}{
				0, 1, 2, 3,
			},
		},
		{
			name:      "no wrap, exactly full",
			size:      5,
			itemCount: 5,
			expectedItems: []interface{}{
				0, 1, 2, 3, 4,
			},
		},
		{
			// The underlying array should contain {5, 1, 2, 3, 4}.
			name:      "wrap, one over",
			size:      5,
			itemCount: 6,
			expectedItems: []interface{}{
				1, 2, 3, 4, 5,
			},
		},
		{
			// The underlying array should contain {5, 6, 2, 3, 4}.
			name:      "wrap, two over",
			size:      5,
			itemCount: 7,
			expectedItems: []interface{}{
				2, 3, 4, 5, 6,
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			buffer, err := NewCircularBuffer(test.size)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for i := 0; i < test.itemCount; i++ {
				buffer.Add(i)
			}

			// List the items in the buffer and check that the list
			// is as expected.
			list := buffer.List()
			if !reflect.DeepEqual(test.expectedItems, list) {
				t.Fatalf("expected %v, got: %v",
					test.expectedItems, list)
			}
		})
	}
}

// TestLatest tests fetching of the last item added to a circular buffer.
func TestLatest(t *testing.T) {
	tests := []struct {
		name string
		size int

		// items is the number of items to add to the buffer.
		items int

		// expectedItem is the value we expect from Latest().
		expectedItem interface{}
	}{
		{
			name:         "no items",
			size:         3,
			items:        0,
			expectedItem: nil,
		},
		{
			name:         "one item",
			size:         3,
			items:        1,
			expectedItem: 0,
		},
		{
			name:         "exactly full",
			size:         3,
			items:        3,
			expectedItem: 2,
		},
		{
			name:         "overflow to index 0",
			size:         3,
			items:        4,
			expectedItem: 3,
		},
		{
			name:         "overflow twice to index 0",
			size:         3,
			items:        7,
			expectedItem: 6,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			//t.Parallel()

			buffer, err := NewCircularBuffer(test.size)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for i := 0; i < test.items; i++ {
				buffer.Add(i)
			}

			latest := buffer.Latest()

			if !reflect.DeepEqual(latest, test.expectedItem) {
				t.Fatalf("expected: %v, got: %v",
					test.expectedItem, latest)
			}
		})
	}
}

// TestCircularBufferConcurrentAccess verifies that peer error writers and RPC
// readers can safely share a buffer without corrupting its order or counters.
func TestCircularBufferConcurrentAccess(t *testing.T) {
	// Arrange: Create a small wrapping buffer and two finite workers behind
	// one start gate. The writer repeatedly wraps the storage while the
	// reader exercises every public observation method against those writes.
	const (
		bufferSize = 8
		iterations = 1000
	)
	buffer, err := NewCircularBuffer(bufferSize)
	require.NoError(t, err)

	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)

	go func() {
		defer workers.Done()
		<-start

		for i := 0; i < iterations; i++ {
			buffer.Add(i)
		}
	}()

	go func() {
		defer workers.Done()
		<-start

		for i := 0; i < iterations; i++ {
			buffer.List()
			buffer.Latest()
			buffer.Total()
		}
	}()

	// Act: Release both workers together and join them before inspecting
	// results, keeping test assertions in the owning goroutine.
	close(start)
	workers.Wait()

	// Assert: Every write is counted and the retained wrapping window is
	// still ordered from oldest to newest after concurrent observations.
	require.Equal(t, iterations, buffer.Total())
	require.Equal(t, iterations-1, buffer.Latest())
	require.Equal(t, []interface{}{
		iterations - 8,
		iterations - 7,
		iterations - 6,
		iterations - 5,
		iterations - 4,
		iterations - 3,
		iterations - 2,
		iterations - 1,
	}, buffer.List())
}
