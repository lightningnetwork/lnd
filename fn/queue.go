package fn

// Queue is a generic queue implementation.
type Queue[T any] struct {
	items []T
}

// NewQueue creates a new Queue.
func NewQueue[T any](startingItems ...T) Queue[T] {
	return Queue[T]{
		items: startingItems,
	}
}

// Enqueue adds one or more an items to the end of the Queue.
func (q *Queue[T]) Enqueue(value ...T) {
	q.items = append(q.items, value...)
}

// Dequeue removes an element from the front of the Queue. If there're no items
// in the queue, then None is returned.
func (q *Queue[T]) Dequeue() Option[T] {
	if len(q.items) == 0 {
		return None[T]()
	}

	value := q.items[0]
	q.items = q.items[1:]

	return Some(value)
}

// Peek returns the first item in the queue without removing it. If the queue
// is empty, then None is returned.
func (q *Queue[T]) Peek() Option[T] {
	if q.IsEmpty() {
		return None[T]()
	}

	return Some(q.items[0])
}

// IsEmpty returns true if the Queue is empty
func (q *Queue[T]) IsEmpty() bool {
	return len(q.items) == 0
}

// Size returns the number of items in the Queue
func (q *Queue[T]) Size() int {
	return len(q.items)
}
