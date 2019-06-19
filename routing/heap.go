package routing

import (
	"container/heap"

	"github.com/lightningnetwork/lnd/channeldb"
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

	// amountToReceive is the amount that should be received by this node.
	// Either as final payment to the final node or as an intermediate
	// amount that includes also the fees for subsequent hops.
	amountToReceive lnwire.MilliSatoshi

	// incomingCltv is the expected cltv value for the incoming htlc of this
	// node. This value does not include the final cltv.
	incomingCltv uint32

	// probability is the probability that from this node onward the route
	// is successful.
	probability float64

	// weight is the cost of the route from this node to the destination.
	// Includes the routing fees and a virtual cost factor to account for
	// time locks.
	weight int64
}

// distanceHeap is a min-distance heap that's used within our path finding
// algorithm to keep track of the "closest" node to our source node.
type distanceHeap struct {
	nodes []nodeWithDist

	// pubkeyIndices maps public keys of nodes to their respective index in
	// the heap. This is used as a way to avoid db lookups by using heap.Fix
	// instead of having duplicate entries on the heap.
	pubkeyIndices map[route.Vertex]int
}

// newDistanceHeap initializes a new distance heap. This is required because
// we must initialize the pubkeyIndices map for path-finding optimizations.
func newDistanceHeap() distanceHeap {
	distHeap := distanceHeap{
		pubkeyIndices: make(map[route.Vertex]int),
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
	n := x.(nodeWithDist)
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
	d.nodes = d.nodes[0 : n-1]
	delete(d.pubkeyIndices, x.node)
	return x
}

// PushOrFix attempts to adjust the position of a given node in the heap.
// If the vertex already exists in the heap, then we must call heap.Fix to
// modify its position and reorder the heap. If the vertex does not already
// exist in the heap, then it is pushed onto the heap. Otherwise, we will end
// up performing more db lookups on the same node in the pathfinding algorithm.
func (d *distanceHeap) PushOrFix(dist nodeWithDist) {
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

// path represents an ordered set of edges which forms an available path from a
// given source node to our destination. During the process of computing the
// KSP's from a source to destination, several path swill be considered in the
// process.
type path struct {
	// dist is the distance from the source node to the destination node
	// that the path requires.
	dist int

	// hops is an ordered list of edges that comprises a potential payment
	// path.
	hops []*channeldb.ChannelEdgePolicy
}

// pathHeap is a min-heap that stores potential paths to be considered within
// our KSP implementation. The heap sorts paths according to their cumulative
// distance from a given source.
type pathHeap struct {
	paths []path
}

// Len returns the number of nodes in the priority queue.
//
// NOTE: This is part of the heap.Interface implementation.
func (p *pathHeap) Len() int { return len(p.paths) }

// Less returns whether the item in the priority queue with index i should sort
// before the item with index j.
//
// NOTE: This is part of the heap.Interface implementation.
func (p *pathHeap) Less(i, j int) bool {
	return p.paths[i].dist < p.paths[j].dist
}

// Swap swaps the nodes at the passed indices in the priority queue.
//
// NOTE: This is part of the heap.Interface implementation.
func (p *pathHeap) Swap(i, j int) {
	p.paths[i], p.paths[j] = p.paths[j], p.paths[i]
}

// Push pushes the passed item onto the priority queue.
//
// NOTE: This is part of the heap.Interface implementation.
func (p *pathHeap) Push(x interface{}) {
	p.paths = append(p.paths, x.(path))
}

// Pop removes the highest priority item (according to Less) from the priority
// queue and returns it.
//
// NOTE: This is part of the heap.Interface implementation.
func (p *pathHeap) Pop() interface{} {
	n := len(p.paths)
	x := p.paths[n-1]
	p.paths = p.paths[0 : n-1]
	return x
}
