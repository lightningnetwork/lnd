package wtclient

import (
	"sync"

	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// sessionCloseMinHeap is a thread-safe min-heap implementation that stores
// sessionCloseItem items and prioritises the item with the lowest block height.
type sessionCloseMinHeap struct {
	queue queue.PriorityQueue
	mu    sync.Mutex
}

// newSessionCloseMinHeap constructs a new sessionCloseMineHeap.
func newSessionCloseMinHeap() *sessionCloseMinHeap {
	return &sessionCloseMinHeap{}
}

// Len returns the length of the queue.
func (h *sessionCloseMinHeap) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.queue.Len()
}

// Empty returns true if the queue is empty.
func (h *sessionCloseMinHeap) Empty() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.queue.Empty()
}

// Push adds an item to the priority queue.
func (h *sessionCloseMinHeap) Push(item *sessionCloseItem) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.queue.Push(item)
}

// Pop removes the top most item from the queue.
func (h *sessionCloseMinHeap) Pop() *sessionCloseItem {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.queue.Empty() {
		return nil
	}

	item := h.queue.Pop()

	return item.(*sessionCloseItem) //nolint:forcetypeassert
}

// Top returns the top most item from the queue without removing it.
func (h *sessionCloseMinHeap) Top() *sessionCloseItem {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.queue.Empty() {
		return nil
	}

	item := h.queue.Top()

	return item.(*sessionCloseItem) //nolint:forcetypeassert
}

// sessionCloseItem represents a session that is ready to be deleted.
type sessionCloseItem struct {
	// sessionID is the ID of the session in question.
	sessionID wtdb.SessionID

	// deleteHeight is the block height after which we can delete the
	// session.
	deleteHeight uint32
}

// Less returns true if the current item's delete height is less than the
// other sessionCloseItem's delete height. This results in lower block heights
// being popped first from the heap.
//
// NOTE: this is part of the queue.PriorityQueueItem interface.
func (s *sessionCloseItem) Less(other queue.PriorityQueueItem) bool {
	o := other.(*sessionCloseItem).deleteHeight //nolint:forcetypeassert

	return s.deleteHeight < o
}

var _ queue.PriorityQueueItem = (*sessionCloseItem)(nil)
