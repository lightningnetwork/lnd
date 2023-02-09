package wtmock

import (
	"container/list"
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// DiskQueueDB is an in-memory implementation of the wtclient.Queue interface.
type DiskQueueDB[T any] struct {
	disk *list.List
	mu   sync.RWMutex
}

// NewQueueDB constructs a new DiskQueueDB.
func NewQueueDB[T any]() wtdb.Queue[T] {
	return &DiskQueueDB[T]{
		disk: list.New(),
	}
}

// Len returns the number of tasks in the queue.
//
// NOTE: This is part of the wtclient.Queue interface.
func (d *DiskQueueDB[T]) Len() (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return uint64(d.disk.Len()), nil
}

// Push adds new T items to the tail of the queue.
//
// NOTE: This is part of the wtclient.Queue interface.
func (d *DiskQueueDB[T]) Push(items ...T) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, item := range items {
		d.disk.PushBack(item)
	}

	return nil
}

// PopUpTo attempts to pop up to n items from the queue. If the queue is empty,
// then ErrEmptyQueue is returned.
//
// NOTE: This is part of the Queue interface.
func (d *DiskQueueDB[T]) PopUpTo(n int) ([]T, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.disk.Len() == 0 {
		return nil, wtdb.ErrEmptyQueue
	}

	num := n
	if d.disk.Len() < n {
		num = d.disk.Len()
	}

	tasks := make([]T, 0, num)
	for i := 0; i < num; i++ {
		e := d.disk.Front()
		task, ok := d.disk.Remove(e).(T)
		if !ok {
			return nil, fmt.Errorf("queue item not of type %T",
				task)
		}

		tasks = append(tasks, task)
	}

	return tasks, nil
}

// PushHead pushes new T items to the head of the queue.
//
// NOTE: This is part of the wtclient.Queue interface.
func (d *DiskQueueDB[T]) PushHead(items ...T) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	for i := len(items) - 1; i >= 0; i-- {
		d.disk.PushFront(items[i])
	}

	return nil
}
