package routing

import "github.com/lightningnetwork/lnd/channeldb"

// nodeWithDist is a helper struct that couples the distance from the current
// source to a node with a pointer to the node itself.
type nodeWithDist struct {
	// dist is the distance to this node from the source node in our
	// current context.
	dist float64

	// node is the vertex itself. This pointer can be used to explore all
	// the outgoing edges (channels) emanating from a node.
	node *channeldb.LightningNode
}

// distanceHeap is a min-distance heap that's used within our path finding
// algorithm to keep track of the "closest" node to our source node.
type distanceHeap struct {
	nodes []nodeWithDist
}

// Len returns the number of nodes in the priority queue.
//
// NOTE: This is part of the heap.Interface implementation.
func (d *distanceHeap) Len() int { return len(d.nodes) }

// Less returns whether the item in the priority queue with index i should sort
// before the item with index j.
//
// NOTE: This is part of the heap.Interface implementation.
func (d *distanceHeap) Less(i, j int) bool {
	return d.nodes[i].dist < d.nodes[j].dist
}

// Swap swaps the nodes at the passed indices in the priority queue.
//
// NOTE: This is part of the heap.Interface implementation.
func (d *distanceHeap) Swap(i, j int) {
	d.nodes[i], d.nodes[j] = d.nodes[j], d.nodes[i]
}

// Push pushes the passed item onto the priority queue.
//
// NOTE: This is part of the heap.Interface implementation.
func (d *distanceHeap) Push(x interface{}) {
	d.nodes = append(d.nodes, x.(nodeWithDist))
}

// Pop removes the highest priority item (according to Less) from the priority
// queue and returns it.
//
// NOTE: This is part of the heap.Interface implementation.
func (d *distanceHeap) Pop() interface{} {
	n := len(d.nodes)
	x := d.nodes[n-1]
	d.nodes = d.nodes[0 : n-1]
	return x
}
