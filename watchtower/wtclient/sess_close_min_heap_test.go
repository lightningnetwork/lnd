package wtclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionCloseMinHeap asserts that the sessionCloseMinHeap behaves as
// expected.
func TestSessionCloseMinHeap(t *testing.T) {
	t.Parallel()

	heap := newSessionCloseMinHeap()
	require.Nil(t, heap.Pop())
	require.Nil(t, heap.Top())
	require.True(t, heap.Empty())
	require.Zero(t, heap.Len())

	// Add an item with height 10.
	item1 := &sessionCloseItem{
		sessionID:    [33]byte{1, 2, 3},
		deleteHeight: 10,
	}

	heap.Push(item1)
	require.Equal(t, item1, heap.Top())
	require.False(t, heap.Empty())
	require.EqualValues(t, 1, heap.Len())

	// Add a bunch more items with heights 1, 2, 6, 11, 6, 30, 9.
	heap.Push(&sessionCloseItem{deleteHeight: 1})
	heap.Push(&sessionCloseItem{deleteHeight: 2})
	heap.Push(&sessionCloseItem{deleteHeight: 6})
	heap.Push(&sessionCloseItem{deleteHeight: 11})
	heap.Push(&sessionCloseItem{deleteHeight: 6})
	heap.Push(&sessionCloseItem{deleteHeight: 30})
	heap.Push(&sessionCloseItem{deleteHeight: 9})

	// Now pop from the queue and assert that the items are returned in
	// ascending order.
	require.EqualValues(t, 1, heap.Pop().deleteHeight)
	require.EqualValues(t, 2, heap.Pop().deleteHeight)
	require.EqualValues(t, 6, heap.Pop().deleteHeight)
	require.EqualValues(t, 6, heap.Pop().deleteHeight)
	require.EqualValues(t, 9, heap.Pop().deleteHeight)
	require.EqualValues(t, 10, heap.Pop().deleteHeight)
	require.EqualValues(t, 11, heap.Pop().deleteHeight)
	require.EqualValues(t, 30, heap.Pop().deleteHeight)
	require.Nil(t, heap.Pop())
	require.Zero(t, heap.Len())
}
