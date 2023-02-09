package wtdb

import "errors"

// ErrEmptyQueue is returned from Pop if there are no items left in the Queue.
var ErrEmptyQueue = errors.New("queue is empty")

// Queue is an interface describing a FIFO queue for any generic type T.
type Queue[T any] interface {
	// Len returns the number of tasks in the queue.
	Len() (uint64, error)

	// Push pushes new T items to the tail of the queue.
	Push(items ...T) error

	// PopUpTo attempts to pop up to n items from the head of the queue. If
	// no more items are in the queue then ErrEmptyQueue is returned.
	PopUpTo(n int) ([]T, error)

	// PushHead pushes new T items to the head of the queue.
	PushHead(items ...T) error
}
