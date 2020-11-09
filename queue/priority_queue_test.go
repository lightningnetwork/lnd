package queue

import (
	"math/rand"
	"testing"
	"time"
)

type testQueueItem struct {
	Value  int
	Expiry time.Time
}

func (e testQueueItem) Less(other PriorityQueueItem) bool {
	return e.Expiry.Before(other.(*testQueueItem).Expiry)
}

func TestExpiryQueue(t *testing.T) {
	// The number of elements we push to the queue.
	count := 100
	// Generate a random permutation of a range [0, count)
	array := rand.Perm(count)
	// t0 holds a reference time point.
	t0 := time.Date(1975, time.April, 5, 12, 0, 0, 0, time.UTC)

	var testQueue PriorityQueue

	if testQueue.Len() != 0 && !testQueue.Empty() {
		t.Fatal("Expected the queue to be empty")
	}

	// Create elements with expiry of t0 + value * second.
	for _, value := range array {
		testQueue.Push(&testQueueItem{
			Value:  value,
			Expiry: t0.Add(time.Duration(value) * time.Second),
		})
	}

	// Now expect that we can retrieve elements in order of their expiry.
	for i := 0; i < count; i++ {
		expectedQueueLen := count - i
		if testQueue.Len() != expectedQueueLen {
			t.Fatalf("Expected the queue len %v, got %v",
				expectedQueueLen, testQueue.Len())
		}

		if testQueue.Empty() {
			t.Fatalf("Did not expect the queue to be empty")
		}

		top := testQueue.Top().(*testQueueItem)
		if top.Value != i {
			t.Fatalf("Expected queue top %v, got %v", i, top.Value)
		}

		popped := testQueue.Pop().(*testQueueItem)
		if popped != top {
			t.Fatalf("Expected queue top %v equal to popped: %v",
				top, popped)
		}
	}

	if testQueue.Len() != 0 || !testQueue.Empty() {
		t.Fatalf("Expected the queue to be empty")
	}
}
