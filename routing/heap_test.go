package routing

import (
	"container/heap"
	prand "math/rand"
	"reflect"
	"sort"
	"testing"
	"time"
)

// TestHeapOrdering ensures that the items inserted into the heap are properly
// retrieved in minimum order of distance.
func TestHeapOrdering(t *testing.T) {
	t.Parallel()

	// First, create a blank heap, we'll use this to push on randomly
	// generated items.
	var nodeHeap distanceHeap

	prand.Seed(time.Now().Unix())

	// Create 100 random entries adding them to the heap created above, but
	// also a list that we'll sort with the entries.
	const numEntries = 100
	sortedEntries := make([]nodeWithDist, 0, numEntries)
	for i := 0; i < numEntries; i++ {
		entry := nodeWithDist{
			dist: prand.Int63(),
		}

		heap.Push(&nodeHeap, entry)
		sortedEntries = append(sortedEntries, entry)
	}

	// Sort the regular slice, we'll compare this against all the entries
	// popped from the heap.
	sort.Slice(sortedEntries, func(i, j int) bool {
		return sortedEntries[i].dist < sortedEntries[j].dist
	})

	// One by one, pop of all the entries from the heap, they should come
	// out in sorted order.
	var poppedEntries []nodeWithDist
	for nodeHeap.Len() != 0 {
		e := heap.Pop(&nodeHeap).(nodeWithDist)
		poppedEntries = append(poppedEntries, e)
	}

	// Finally, ensure that the items popped from the heap and the items we
	// sorted are identical at this rate.
	if !reflect.DeepEqual(poppedEntries, sortedEntries) {
		t.Fatalf("items don't match: expected %v, got %v", sortedEntries,
			poppedEntries)
	}
}
