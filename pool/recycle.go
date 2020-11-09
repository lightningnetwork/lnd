package pool

import (
	"time"

	"github.com/lightningnetwork/lnd/queue"
)

// Recycler is an interface that allows an object to be reclaimed without
// needing to be returned to the runtime.
type Recycler interface {
	// Recycle resets the object to its default state.
	Recycle()
}

// Recycle is a generic queue for recycling objects implementing the Recycler
// interface. It is backed by an underlying queue.GCQueue, and invokes the
// Recycle method on returned objects before returning them to the queue.
type Recycle struct {
	queue *queue.GCQueue
}

// NewRecycle initializes a fresh Recycle instance.
func NewRecycle(newItem func() interface{}, returnQueueSize int,
	gcInterval, expiryInterval time.Duration) *Recycle {

	return &Recycle{
		queue: queue.NewGCQueue(
			newItem, returnQueueSize,
			gcInterval, expiryInterval,
		),
	}
}

// Take returns an element from the pool.
func (r *Recycle) Take() interface{} {
	return r.queue.Take()
}

// Return returns an item implementing the Recycler interface to the pool. The
// Recycle method is invoked before returning the item to improve performance
// and utilization under load.
func (r *Recycle) Return(item Recycler) {
	// Recycle the item to ensure that a dirty instance is never offered
	// from Take. The call is done here so that the CPU cycles spent
	// clearing the buffer are owned by the caller, and not by the queue
	// itself. This makes the queue more likely to be available to deliver
	// items in the free list.
	item.Recycle()

	r.queue.Return(item)
}
