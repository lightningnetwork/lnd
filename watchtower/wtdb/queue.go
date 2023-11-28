package wtdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// queueMainBkt will hold the main queue contents. It will have the
	// following structure:
	// 			=> oldestIndexKey => oldest index
	//                      => nextIndexKey => newest index
	//		        => itemsBkt => <index> -> item
	//
	// Any items added to the queue via Push, will be added to this queue.
	// Items will only be popped from this queue if the head queue is empty.
	queueMainBkt = []byte("queue-main")

	// queueHeadBkt will hold the items that have been pushed to the head
	// of the queue. It will have the following structure:
	// 			=> oldestIndexKey => oldest index
	//                      => nextIndexKey => newest index
	//		        => itemsBkt => <index> -> item
	//
	// If PushHead is called with a new set of items, then first all
	// remaining items in the head queue will be popped and added ot the
	// given set of items. Then, once the head queue is empty, the set of
	// items will be pushed to the queue. If this queue is not empty, then
	// Pop will pop items from this queue before popping from the main
	// queue.
	queueHeadBkt = []byte("queue-head")

	// itemsBkt is a sub-bucket of both the main and head queue storing:
	// 		index -> encoded item
	itemsBkt = []byte("items")

	// oldestIndexKey is a key of both the main and head queue storing the
	// index of the item at the head of the queue.
	oldestIndexKey = []byte("oldest-index")

	// nextIndexKey is a key of both the main and head queue storing the
	// index of the item at the tail of the queue.
	nextIndexKey = []byte("next-index")

	// ErrEmptyQueue is returned from Pop if there are no items left in
	// the queue.
	ErrEmptyQueue = errors.New("queue is empty")
)

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

// Serializable is an interface must be satisfied for any type that the
// DiskQueueDB should handle.
type Serializable interface {
	Encode(w io.Writer) error
	Decode(r io.Reader) error
}

// DiskQueueDB is a generic Bolt DB implementation of the Queue interface.
type DiskQueueDB[T Serializable] struct {
	db          kvdb.Backend
	topLevelBkt []byte
	constructor func() T
	onItemWrite func(tx kvdb.RwTx, item T) error
}

// A compile-time check to ensure that DiskQueueDB implements the Queue
// interface.
var _ Queue[Serializable] = (*DiskQueueDB[Serializable])(nil)

// NewQueueDB constructs a new DiskQueueDB. A queueBktName must be provided so
// that the DiskQueueDB can create its own namespace in the bolt db.
func NewQueueDB[T Serializable](db kvdb.Backend, queueBktName []byte,
	constructor func() T,
	onItemWrite func(tx kvdb.RwTx, item T) error) Queue[T] {

	return &DiskQueueDB[T]{
		db:          db,
		topLevelBkt: queueBktName,
		constructor: constructor,
		onItemWrite: onItemWrite,
	}
}

// Len returns the number of tasks in the queue.
//
// NOTE: This is part of the Queue interface.
func (d *DiskQueueDB[T]) Len() (uint64, error) {
	var res uint64
	err := kvdb.View(d.db, func(tx kvdb.RTx) error {
		var err error
		res, err = d.len(tx)

		return err
	}, func() {
		res = 0
	})
	if err != nil {
		return 0, err
	}

	return res, nil
}

// Push adds a T to the tail of the queue.
//
// NOTE: This is part of the Queue interface.
func (d *DiskQueueDB[T]) Push(items ...T) error {
	return d.db.Update(func(tx walletdb.ReadWriteTx) error {
		for _, item := range items {
			err := d.addItem(tx, queueMainBkt, item)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// PopUpTo attempts to pop up to n items from the queue. If the queue is empty,
// then ErrEmptyQueue is returned.
//
// NOTE: This is part of the Queue interface.
func (d *DiskQueueDB[T]) PopUpTo(n int) ([]T, error) {
	var items []T

	err := d.db.Update(func(tx walletdb.ReadWriteTx) error {
		// Get the number of items in the queue.
		l, err := d.len(tx)
		if err != nil {
			return err
		}

		// If there are no items, then we are done.
		if l == 0 {
			return ErrEmptyQueue
		}

		// If the number of items in the queue is less than the maximum
		// specified by the caller, then set the maximum to the number
		// of items that there actually are.
		num := n
		if l < uint64(n) {
			num = int(l)
		}

		// Pop the specified number of items off of the queue.
		items = make([]T, 0, num)
		for i := 0; i < num; i++ {
			item, err := d.pop(tx)
			if err != nil {
				return err
			}

			items = append(items, item)
		}

		return err
	}, func() {
		items = nil
	})
	if err != nil {
		return nil, err
	}

	return items, nil
}

// PushHead pushes new T items to the head of the queue. For this implementation
// of the Queue interface, this will require popping all items currently in the
// head queue and adding them after first adding the given list of items. Care
// should thus be taken to never have an unbounded number of items in the head
// queue.
//
// NOTE: This is part of the Queue interface.
func (d *DiskQueueDB[T]) PushHead(items ...T) error {
	return d.db.Update(func(tx walletdb.ReadWriteTx) error {
		// Determine how many items are still in the head queue.
		numHead, err := d.numItems(tx, queueHeadBkt)
		if err != nil {
			return err
		}

		// Create a new in-memory list that will contain all the new
		// items along with the items currently in the queue.
		itemList := make([]T, 0, int(numHead)+len(items))

		// Insert all the given items into the list first since these
		// should be at the head of the queue.
		itemList = append(itemList, items...)

		// Now, read out all the items that are currently in the
		// persisted head queue and add them to the back of the list
		// of items to be added.
		for {
			t, err := d.nextItem(tx, queueHeadBkt)
			if errors.Is(err, ErrEmptyQueue) {
				break
			} else if err != nil {
				return err
			}

			itemList = append(itemList, t)
		}

		// Now the head queue is empty, the items can be pushed to the
		// queue.
		for _, item := range itemList {
			err := d.addItem(tx, queueHeadBkt, item)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// pop gets the next T item from the head of the queue. If no more items are in
// the queue then ErrEmptyQueue is returned.
func (d *DiskQueueDB[T]) pop(tx walletdb.ReadWriteTx) (T, error) {
	// First, check if there are items left in the head queue.
	item, err := d.nextItem(tx, queueHeadBkt)

	// No error means that an item was found in the head queue.
	if err == nil {
		return item, nil
	}

	// Else, if error is not ErrEmptyQueue, then return the error.
	if !errors.Is(err, ErrEmptyQueue) {
		return item, err
	}

	// Otherwise, the head queue is empty, so we now check if there are
	// items in the main queue.
	return d.nextItem(tx, queueMainBkt)
}

// addItem adds the given item to the back of the given queue.
func (d *DiskQueueDB[T]) addItem(tx kvdb.RwTx, queueName []byte, item T) error {
	var (
		namespacedBkt = tx.ReadWriteBucket(d.topLevelBkt)
		err           error
	)
	if namespacedBkt == nil {
		namespacedBkt, err = tx.CreateTopLevelBucket(d.topLevelBkt)
		if err != nil {
			return err
		}
	}

	mainTasksBucket, err := namespacedBkt.CreateBucketIfNotExists(
		cTaskQueue,
	)
	if err != nil {
		return err
	}

	bucket, err := mainTasksBucket.CreateBucketIfNotExists(queueName)
	if err != nil {
		return err
	}

	if d.onItemWrite != nil {
		err = d.onItemWrite(tx, item)
		if err != nil {
			return err
		}
	}

	// Find the index to use for placing this new item at the back of the
	// queue.
	var nextIndex uint64
	nextIndexB := bucket.Get(nextIndexKey)
	if nextIndexB != nil {
		nextIndex, err = readBigSize(nextIndexB)
		if err != nil {
			return err
		}
	} else {
		nextIndexB, err = writeBigSize(0)
		if err != nil {
			return err
		}
	}

	tasksBucket, err := bucket.CreateBucketIfNotExists(itemsBkt)
	if err != nil {
		return err
	}

	var buff bytes.Buffer
	err = item.Encode(&buff)
	if err != nil {
		return err
	}

	// Put the new task in the assigned index.
	err = tasksBucket.Put(nextIndexB, buff.Bytes())
	if err != nil {
		return err
	}

	// Increment the next-index counter.
	nextIndex++
	nextIndexB, err = writeBigSize(nextIndex)
	if err != nil {
		return err
	}

	return bucket.Put(nextIndexKey, nextIndexB)
}

// nextItem pops an item of the queue identified by the given namespace. If
// there are no items on the queue then ErrEmptyQueue is returned.
func (d *DiskQueueDB[T]) nextItem(tx kvdb.RwTx, queueName []byte) (T, error) {
	task := d.constructor()

	namespacedBkt := tx.ReadWriteBucket(d.topLevelBkt)
	if namespacedBkt == nil {
		return task, ErrEmptyQueue
	}

	mainTasksBucket := namespacedBkt.NestedReadWriteBucket(cTaskQueue)
	if mainTasksBucket == nil {
		return task, ErrEmptyQueue
	}

	bucket, err := mainTasksBucket.CreateBucketIfNotExists(queueName)
	if err != nil {
		return task, err
	}

	// Get the index of the tail of the queue.
	var nextIndex uint64
	nextIndexB := bucket.Get(nextIndexKey)
	if nextIndexB != nil {
		nextIndex, err = readBigSize(nextIndexB)
		if err != nil {
			return task, err
		}
	}

	// Get the index of the head of the queue.
	var oldestIndex uint64
	oldestIndexB := bucket.Get(oldestIndexKey)
	if oldestIndexB != nil {
		oldestIndex, err = readBigSize(oldestIndexB)
		if err != nil {
			return task, err
		}
	} else {
		oldestIndexB, err = writeBigSize(0)
		if err != nil {
			return task, err
		}
	}

	// If the head and tail are equal, then there are no items in the queue.
	if oldestIndex == nextIndex {
		// Take this opportunity to reset both indexes to zero.
		zeroIndexB, err := writeBigSize(0)
		if err != nil {
			return task, err
		}

		err = bucket.Put(oldestIndexKey, zeroIndexB)
		if err != nil {
			return task, err
		}

		err = bucket.Put(nextIndexKey, zeroIndexB)
		if err != nil {
			return task, err
		}

		return task, ErrEmptyQueue
	}

	// Otherwise, pop the item at the oldest index.
	tasksBucket := bucket.NestedReadWriteBucket(itemsBkt)
	if tasksBucket == nil {
		return task, fmt.Errorf("client-tasks bucket not found")
	}

	item := tasksBucket.Get(oldestIndexB)
	if item == nil {
		return task, fmt.Errorf("no task found under index")
	}

	err = tasksBucket.Delete(oldestIndexB)
	if err != nil {
		return task, err
	}

	// Increment the oldestIndex value so that it now points to the new
	// oldest item.
	oldestIndex++
	oldestIndexB, err = writeBigSize(oldestIndex)
	if err != nil {
		return task, err
	}

	err = bucket.Put(oldestIndexKey, oldestIndexB)
	if err != nil {
		return task, err
	}

	if err = task.Decode(bytes.NewBuffer(item)); err != nil {
		return task, err
	}

	return task, nil
}

// len returns the number of items in the queue. This will be the addition of
// the number of items in the main queue and the number in the head queue.
func (d *DiskQueueDB[T]) len(tx kvdb.RTx) (uint64, error) {
	numMain, err := d.numItems(tx, queueMainBkt)
	if err != nil {
		return 0, err
	}

	numHead, err := d.numItems(tx, queueHeadBkt)
	if err != nil {
		return 0, err
	}

	return numMain + numHead, nil
}

// numItems returns the number of items in the given queue.
func (d *DiskQueueDB[T]) numItems(tx kvdb.RTx, queueName []byte) (uint64,
	error) {

	// Get the queue bucket at the correct namespace.
	namespacedBkt := tx.ReadBucket(d.topLevelBkt)
	if namespacedBkt == nil {
		return 0, nil
	}

	mainTasksBucket := namespacedBkt.NestedReadBucket(cTaskQueue)
	if mainTasksBucket == nil {
		return 0, nil
	}

	bucket := mainTasksBucket.NestedReadBucket(queueName)
	if bucket == nil {
		return 0, nil
	}

	var (
		oldestIndex uint64
		nextIndex   uint64
		err         error
	)

	// Get the next index key.
	nextIndexB := bucket.Get(nextIndexKey)
	if nextIndexB != nil {
		nextIndex, err = readBigSize(nextIndexB)
		if err != nil {
			return 0, err
		}
	}

	// Get the oldest index.
	oldestIndexB := bucket.Get(oldestIndexKey)
	if oldestIndexB != nil {
		oldestIndex, err = readBigSize(oldestIndexB)
		if err != nil {
			return 0, err
		}
	}

	return nextIndex - oldestIndex, nil
}
