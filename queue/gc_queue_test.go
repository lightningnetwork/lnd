package queue_test

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/queue"
)

// testItem is an item type we'll be using to test the GCQueue.
type testItem uint32

// TestGCQueueGCCycle asserts that items that are kept in the GCQueue past their
// expiration will be released by a subsequent gc cycle.
func TestGCQueueGCCycle(t *testing.T) {
	t.Parallel()

	const (
		gcInterval     = time.Second
		expiryInterval = 250 * time.Millisecond
		numItems       = 6
	)

	newItem := func() interface{} { return new(testItem) }

	bp := queue.NewGCQueue(newItem, 100, gcInterval, expiryInterval)

	// Take numItems items from the queue, and immediately return them.
	// Returning the items will trigger the gc ticker to start.
	itemSet1 := takeN(t, bp, numItems)
	returnAll(bp, itemSet1)

	// Allow enough time for all expired items to be released by the queue.
	<-time.After(gcInterval + expiryInterval)

	// Take another set of numItems items from the queue.
	itemSet2 := takeN(t, bp, numItems)

	// Since the gc ticker should have elapsed, we expect the intersection
	// of sets 1 and 2 to be empty.
	for item := range itemSet2 {
		if _, ok := itemSet1[item]; ok {
			t.Fatalf("items taken should not have been reused")
		}
	}
}

// TestGCQueuePartialGCCycle asserts that the GCQueue will only garbage collect
// the items in its queue that have fully expired. We test this by adding items
// into the queue such that the garbage collection will occur before the items
// expire. Taking items after the gc cycle should return the items that were not
// released by the gc cycle.
func TestGCQueuePartialGCCycle(t *testing.T) {
	t.Parallel()

	const (
		gcInterval     = time.Second
		expiryInterval = 250 * time.Millisecond
		numItems       = 6
	)

	newItem := func() interface{} { return new(testItem) }

	bp := queue.NewGCQueue(newItem, 100, gcInterval, expiryInterval)

	// Take numItems items from the gc queue.
	itemSet1 := takeN(t, bp, numItems)

	// Immediately return half of the items, and construct a set of items
	// consisting of the half that were not returned.
	halfItemSet1 := returnN(t, bp, itemSet1, numItems/2)

	// Wait long enough to ensure that adding subsequent items will not be
	// released in the next gc cycle.
	<-time.After(gcInterval - expiryInterval/2)

	// Return the remaining items from itemSet1.
	returnAll(bp, halfItemSet1)

	// Wait until the gc cycle as done a sweep of the items and released all
	// those that have expired.
	<-time.After(expiryInterval / 2)

	// Retrieve numItems items from the gc queue.
	itemSet2 := takeN(t, bp, numItems)

	// Tally the number of items returned from Take that are in the second
	// half of items returned.
	var numReused int
	for item := range itemSet2 {
		if _, ok := halfItemSet1[item]; ok {
			numReused++
		}
	}

	// We expect the number of reused items to be equal to half numItems.
	if numReused != numItems/2 {
		t.Fatalf("expected %d items to be reused, got %d",
			numItems/2, numReused)
	}
}

// takeN draws n items from the provided GCQueue. This method also asserts that
// n unique items are drawn, and then returns the resulting set.
func takeN(t *testing.T, q *queue.GCQueue, n int) map[interface{}]struct{} {
	t.Helper()

	items := make(map[interface{}]struct{})
	for i := 0; i < n; i++ {
		// Wait a small duration to ensure the tests behave reliable,
		// and don't activate the non-blocking case unintentionally.
		<-time.After(time.Millisecond)

		items[q.Take()] = struct{}{}
	}

	if len(items) != n {
		t.Fatalf("items taken from gc queue should be distinct, "+
			"want %d unique items, got %d", n, len(items))
	}

	return items
}

// returnAll returns the items of the given set back to the GCQueue.
func returnAll(q *queue.GCQueue, items map[interface{}]struct{}) {
	for item := range items {
		q.Return(item)

		// Wait a small duration to ensure the tests behave reliable,
		// and don't activate the non-blocking case unintentionally.
		<-time.After(time.Millisecond)
	}
}

// returnN returns n items at random from the set of items back to the GCQueue.
// This method fails if the set's cardinality is smaller than n.
func returnN(t *testing.T, q *queue.GCQueue,
	items map[interface{}]struct{}, n int) map[interface{}]struct{} {

	t.Helper()

	var remainingItems = make(map[interface{}]struct{})
	var numReturned int
	for item := range items {
		if numReturned < n {
			q.Return(item)
			numReturned++

			// Wait a small duration to ensure the tests behave
			// reliable, and don't activate the non-blocking case
			// unintentionally.
			<-time.After(time.Millisecond)
		} else {
			remainingItems[item] = struct{}{}
		}
	}

	if numReturned < n {
		t.Fatalf("insufficient number of items to return, need %d, "+
			"got %d", n, numReturned)
	}

	return remainingItems
}
