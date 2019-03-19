package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// nodeWithDist is a helper struct that couples the distance from the current
// source to a node with a pointer to the node itself.
type nodeWithDist struct {
	// dist is the distance to this node from the source node in our
	// current context.
	dist int64

	// node is the vertex itself. This pointer can be used to explore all
	// the outgoing edges (channels) emanating from a node.
	node *channeldb.LightningNode

	// amountToReceive is the amount that should be received by this node.
	// Either as final payment to the final node or as an intermediate
	// amount that includes also the fees for subsequent hops.
	amountToReceive lnwire.MilliSatoshi

	// incomingCltv is the expected cltv value for the incoming htlc of this
	// node. This value does not include the final cltv.
	incomingCltv uint32

	// fee is the fee that this node is charging for forwarding.
	fee lnwire.MilliSatoshi

	// probability is the probability that from this node onward the route
	// is successful.
	probability float64

	// weight is the cost of the route from this node to the destination.
	weight int64
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
