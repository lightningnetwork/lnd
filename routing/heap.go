package routing

import (
	"container/heap"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// nodeWithDist is a helper struct that couples the distance from the current
// source to a node with a pointer to the node itself.
type nodeWithDist struct {
	// dist is the distance to this node from the source node in our
	// current context.
	dist int64

	// node is the vertex itself. This can be used to explore all the
	// outgoing edges (channels) emanating from a node.
	node route.Vertex

	// netAmountReceived is the amount that should be received by this node.
	// Either as final payment to the final node or as an intermediate
	// amount that includes also the fees for subsequent hops. This node's
	// inbound fee is already subtracted from the htlc amount - if
	// applicable.
	netAmountReceived lnwire.MilliSatoshi

	// outboundFee is the fee that this node charges on the outgoing
	// channel.
	outboundFee lnwire.MilliSatoshi

	// incomingCltv is the expected absolute expiry height for the incoming
	// htlc of this node. This value should already include the final cltv
	// delta.
	incomingCltv int32

	// probability is the probability that from this node onward the route
	// is successful.
	probability float64

	// weight is the cost of the route from this node to the destination.
	// Includes the routing fees and a virtual cost factor to account for
	// time locks.
	weight int64

	// nextHop is the edge this route comes from.
	nextHop *unifiedEdge

	// routingInfoSize is the total size requirement for the payloads field
	// in the onion packet from this hop towards the final destination.
	routingInfoSize uint64
}

// distanceHeap is a min-distance heap that's used within our path finding
// algorithm to keep track of the "closest" node to our source node.
type distanceHeap struct {
	nodes []*nodeWithDist

	// pubkeyIndices maps public keys of nodes to their respective index in
	// the heap. This is used as a way to avoid db lookups by using heap.Fix
	// instead of having duplicate entries on the heap.
	pubkeyIndices map[route.Vertex]int
}

// newDistanceHeap initializes a new distance heap. This is required because
// we must initialize the pubkeyIndices map for path-finding optimizations.
func newDistanceHeap(numNodes int) distanceHeap {
	distHeap := distanceHeap{
		pubkeyIndices: make(map[route.Vertex]int, numNodes),
		nodes:         make([]*nodeWithDist, 0, numNodes),
	}

	return distHeap
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
	// If distances are equal, tie break on probability.
	if d.nodes[i].dist == d.nodes[j].dist {
		return d.nodes[i].probability > d.nodes[j].probability
	}

	return d.nodes[i].dist < d.nodes[j].dist
}

// Swap swaps the nodes at the passed indices in the priority queue.
//
// NOTE: This is part of the heap.Interface implementation.
func (d *distanceHeap) Swap(i, j int) {
	d.nodes[i], d.nodes[j] = d.nodes[j], d.nodes[i]
	d.pubkeyIndices[d.nodes[i].node] = i
	d.pubkeyIndices[d.nodes[j].node] = j
}

// Push pushes the passed item onto the priority queue.
//
// NOTE: This is part of the heap.Interface implementation.
func (d *distanceHeap) Push(x interface{}) {
	n := x.(*nodeWithDist)
	d.nodes = append(d.nodes, n)
	d.pubkeyIndices[n.node] = len(d.nodes) - 1
}

// Pop removes the highest priority item (according to Less) from the priority
// queue and returns it.
//
// NOTE: This is part of the heap.Interface implementation.
func (d *distanceHeap) Pop() interface{} {
	n := len(d.nodes)
	x := d.nodes[n-1]
	d.nodes[n-1] = nil
	d.nodes = d.nodes[0 : n-1]
	delete(d.pubkeyIndices, x.node)
	return x
}

// PushOrFix attempts to adjust the position of a given node in the heap.
// If the vertex already exists in the heap, then we must call heap.Fix to
// modify its position and reorder the heap. If the vertex does not already
// exist in the heap, then it is pushed onto the heap. Otherwise, we will end
// up performing more db lookups on the same node in the pathfinding algorithm.
func (d *distanceHeap) PushOrFix(dist *nodeWithDist) {
	index, ok := d.pubkeyIndices[dist.node]
	if !ok {
		heap.Push(d, dist)
		return
	}

	// Change the value at the specified index.
	d.nodes[index] = dist

	// Call heap.Fix to reorder the heap.
	heap.Fix(d, index)
}
