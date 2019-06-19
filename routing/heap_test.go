package routing

import (
	"container/heap"
	prand "math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/lightningnetwork/lnd/routing/route"
)

// TestHeapOrdering ensures that the items inserted into the heap are properly
// retrieved in minimum order of distance.
func TestHeapOrdering(t *testing.T) {
	t.Parallel()

	// First, create a blank heap, we'll use this to push on randomly
	// generated items.
	nodeHeap := newDistanceHeap()

	prand.Seed(1)

	// Create 100 random entries adding them to the heap created above, but
	// also a list that we'll sort with the entries.
	const numEntries = 100
	sortedEntries := make([]nodeWithDist, 0, numEntries)
	for i := 0; i < numEntries; i++ {
		var pubKey [33]byte
		prand.Read(pubKey[:])

		entry := nodeWithDist{
			node: route.Vertex(pubKey),
			dist: prand.Int63(),
		}

		// Use the PushOrFix method for the initial push to test the scenario
		// where entry doesn't exist on the heap.
		nodeHeap.PushOrFix(entry)

		// Re-generate this entry's dist field
		entry.dist = prand.Int63()

		// Reorder the heap with a PushOrFix call.
		nodeHeap.PushOrFix(entry)

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

	// Assert that the pubkeyIndices map is empty after popping all of the
	// items off of it.
	if len(nodeHeap.pubkeyIndices) != 0 {
		t.Fatalf("there are still %d pubkeys in the pubkeyIndices map",
			len(nodeHeap.pubkeyIndices))
	}

	// Finally, ensure that the items popped from the heap and the items we
	// sorted are identical at this rate.
	if !reflect.DeepEqual(poppedEntries, sortedEntries) {
		t.Fatalf("items don't match: expected %v, got %v", sortedEntries,
			poppedEntries)
	}
}
