package routing

import (
	"math"

	"container/heap"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

const (
	// HopLimit is the maximum number hops that is permissible as a route.
	// Any potential paths found that lie above this limit will be rejected
	// with an error. This value is computed using the current fixed-size
	// packet length of the Sphinx construction.
	HopLimit = 20

	// infinity is used as a starting distance in our shortest path search.
	infinity = math.MaxFloat64
)

// ChannelHop is an intermediate hop within the network with a greater
// multi-hop payment route. This struct contains the relevant routing policy of
// the particular edge, as well as the total capacity, and origin chain of the
// channel itself.
type ChannelHop struct {
	// Capacity is the total capacity of the channel being traversed. This
	// value is expressed for stability in satoshis.
	Capacity btcutil.Amount

	// Chain is a 32-byte has that denotes the base blockchain network of
	// the channel. The 32-byte hash is the "genesis" block of the
	// blockchain, or the very first block in the chain.
	//
	// TODO(roasbeef): store chain within edge info/policy in database.
	Chain chainhash.Hash

	*channeldb.ChannelEdgePolicy
}

// Hop represents the forwarding details at a particular position within the
// final route. This struct houses the values necessary to create the HTLC
// which will travel along this hop, and also encode the per-hop payload
// included within the Sphinx packet.
type Hop struct {
	// Channel is the active payment channel edge that this hop will travel
	// along.
	Channel *ChannelHop

	// TimeLockDelta is the delta that this hop will subtract from the HTLC
	// before extending it to the next hop in the route.
	TimeLockDelta uint16

	// AmtToForward is the amount that this hop will forward to the next
	// hop. This value is less than the value that the incoming HTLC
	// carries as a fee will be subtracted by the hop.
	AmtToForward btcutil.Amount

	// Fee is the total fee that this hop will subtract from the incoming
	// payment, this difference nets the hop fees for forwarding the
	// payment.
	Fee btcutil.Amount
}

// computeFee computes the fee to forward an HTLC of `amt` satoshis over the
// passed active payment channel. This value is currently computed as specified
// in BOLT07, but will likely change in the near future.
func computeFee(amt btcutil.Amount, edge *ChannelHop) btcutil.Amount {
	return edge.FeeBaseMSat + (amt*edge.FeeProportionalMillionths)/1000000
}

// isSamePath returns true if path1 and path2 travel through the exact same
// edges, and false otherwise.
func isSamePath(path1, path2 []*ChannelHop) bool {
	for i := 0; i < len(path1); i++ {
		if path1[i].ChannelID != path2[i].ChannelID {
			return false
		}
	}

	return true
}

// Route represents a path through the channel graph which runs over one or
// more channels in succession. This struct carries all the information
// required to craft the Sphinx onion packet, and send the payment along the
// first hop in the path. A route is only selected as valid if all the channels
// have sufficient capacity to carry the initial payment amount after fees are
// accounted for.
type Route struct {
	// TotalTimeLock is the cumulative (final) time lock across the entire
	// route. This is the CLTV value that should be extended to the first
	// hop in the route. All other hops will decrement the time-lock as
	// advertised, leaving enough time for all hops to wait for or present
	// the payment preimage to complete the payment.
	TotalTimeLock uint32

	// TotalFees is the sum of the fees paid at each hop within the final
	// route. In the case of a one-hop payment, this value will be zero as
	// we don't need to pay a fee it ourself.
	TotalFees btcutil.Amount

	// TotalAmount is the total amount of funds required to complete a
	// payment over this route. This value includes the cumulative fees at
	// each hop. As a result, the HTLC extended to the first-hop in the
	// route will need to have at least this many satoshis, otherwise the
	// route will fail at an intermediate node due to an insufficient
	// amount of fees.
	TotalAmount btcutil.Amount

	// Hops contains details concerning the specific forwarding details at
	// each hop.
	Hops []*Hop
}

// newRoute returns a fully valid route between the source and target that's
// capable of supporting a payment of `amtToSend` after fees are fully
// computed. If the route is too long, or the selected path cannot support the
// fully payment including fees, then a non-nil error is returned.
//
// NOTE: The passed slice of ChannelHops MUST be sorted in forward order: from
// the source to the target node of the path finding attempt.
func newRoute(amtToSend btcutil.Amount, pathEdges []*ChannelHop) (*Route, error) {
	route := &Route{
		Hops: make([]*Hop, len(pathEdges)),
	}

	// The running amount is the total amount of satoshis required at this
	// point in the route. We start this value at the amount we want to
	// send to the destination. This value will then get successively
	// larger as we compute the fees going backwards.
	runningAmt := amtToSend
	pathLength := len(pathEdges)
	for i := pathLength - 1; i >= 0; i-- {
		edge := pathEdges[i]

		// Now we create the hop struct for this point in the route.
		// The amount to forward is the running amount, and we compute
		// the required fee based on this amount.
		nextHop := &Hop{
			Channel:       edge,
			AmtToForward:  runningAmt,
			Fee:           computeFee(runningAmt, edge),
			TimeLockDelta: edge.TimeLockDelta,
		}
		edge.Node.PubKey.Curve = nil

		// As a sanity check, we ensure that the selected channel has
		// enough capacity to forward the required amount which
		// includes the fee dictated at each hop.
		if nextHop.AmtToForward > nextHop.Channel.Capacity {
			return nil, ErrInsufficientCapacity
		}

		// We don't pay any fees to ourselves on the first-hop channel,
		// so we don't tally up the running fee and amount.
		if i != len(pathEdges)-1 {
			// For a node to forward an HTLC, then following
			// inequality most hold true: amt_in - fee >=
			// amt_to_forward. Therefore we add the fee this node
			// consumes in order to calculate the amount that it
			// show be forwarded by the prior node which is the
			// next hop in our loop.
			runningAmt += nextHop.Fee

			// Next we tally the total fees (thus far) in the
			// route, and also accumulate the total timelock in the
			// route by adding the node's time lock delta which is
			// the amount of blocks it'll subtract from the
			// incoming time lock.
			route.TotalFees += nextHop.Fee
		} else {
			nextHop.Fee = 0
		}

		route.TotalTimeLock += uint32(nextHop.TimeLockDelta)

		route.Hops[i] = nextHop
	}

	// The total amount required for this route will be the value the
	// source extends to the first hop in the route.
	route.TotalAmount = runningAmt

	return route, nil
}

// vertex is a simple alias for the serialization of a compressed Bitcoin
// public key.
type vertex [33]byte

// newVertex returns a new vertex given a public key.
func newVertex(pub *btcec.PublicKey) vertex {
	var v vertex
	copy(v[:], pub.SerializeCompressed())
	return v
}

// edgeWithPrev is a helper struct used in path finding that couples an
// directional edge with the node's ID in the opposite direction.
type edgeWithPrev struct {
	edge     *ChannelHop
	prevNode *btcec.PublicKey
}

// edgeWeight computes the weight of an edge. This value is used when searching
// for the shortest path within the channel graph between two nodes. Currently
// this is just 1 + the cltv delta value required at this hop, this value
// should be tuned with experimental and empirical data.
//
// TODO(roasbeef): compute robust weight metric
func edgeWeight(e *channeldb.ChannelEdgePolicy) float64 {
	return float64(1 + e.TimeLockDelta)
}

// findPath attempts to find a path from the source node within the
// ChannelGraph to the target node that's capable of supporting a payment of
// `amt` value. The current approach implemented is modified version of
// Dijkstra's algorithm to find a single shortest path between the source node
// and the destination. The distance metric used for edges is related to the
// time-lock+fee costs along a particular edge. If a path is found, this
// function returns a slice of ChannelHop structs which encoded the chosen path
// from the target to the source.
func findPath(graph *channeldb.ChannelGraph, sourceNode *channeldb.LightningNode,
	target *btcec.PublicKey, ignoredNodes map[vertex]struct{},
	ignoredEdges map[uint64]struct{}, amt btcutil.Amount) ([]*ChannelHop, error) {

	// First we'll initialize an empty heap which'll help us to quickly
	// locate the next edge we should visit next during our graph
	// traversal.
	var nodeHeap distanceHeap

	// For each node/vertex the graph we create an entry in the distance
	// map for the node set with a distance of "infinity".  We also mark
	// add the node to our set of unvisited nodes.
	distance := make(map[vertex]nodeWithDist)
	if err := graph.ForEachNode(func(node *channeldb.LightningNode) error {
		// TODO(roasbeef): with larger graph can just use disk seeks
		// with a visited map
		distance[newVertex(node.PubKey)] = nodeWithDist{
			dist: infinity,
			node: node,
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// To start, we add the source of our path finding attempt to the
	// distance map with with a distance of 0. This indicates our starting
	// point in the graph traversal.
	sourceVertex := newVertex(sourceNode.PubKey)
	distance[sourceVertex] = nodeWithDist{
		dist: 0,
		node: sourceNode,
	}

	// To start, our source node will the sole item within our distance
	// heap.
	heap.Push(&nodeHeap, distance[sourceVertex])

	// We'll use this map as a series of "previous" hop pointers. So to get
	// to `vertex` we'll take the edge that it's mapped to within `prev`.
	prev := make(map[vertex]edgeWithPrev)

	for nodeHeap.Len() != 0 {
		// Fetch the node within the smallest distance from our source
		// from the heap.
		partialPath := heap.Pop(&nodeHeap).(nodeWithDist)
		bestNode := partialPath.node

		// If we've reached our target (or we don't have any outgoing
		// edges), then we're done here and can exit the graph
		// traversal early.
		if bestNode.PubKey.IsEqual(target) {
			break
		}

		// Now that we've found the next potential step to take we'll
		// examine all the outgoing edge (channels) from this node to
		// further our graph traversal.
		pivot := newVertex(bestNode.PubKey)
		err := bestNode.ForEachChannel(nil, func(edgeInfo *channeldb.ChannelEdgeInfo,
			edge *channeldb.ChannelEdgePolicy) error {

			v := newVertex(edge.Node.PubKey)

			// If this vertex or edge has been black listed, then
			// we'll skip exploring this edge during this
			// iteration.
			if _, ok := ignoredNodes[v]; ok {
				return nil
			}
			if _, ok := ignoredEdges[edge.ChannelID]; ok {
				return nil
			}

			// Compute the tentative distance to this new
			// channel/edge which is the distance to our current
			// pivot node plus the weight of this edge.
			tempDist := distance[pivot].dist + edgeWeight(edge)

			// If this new tentative distance is better than the
			// current best known distance to this node, then we
			// record the new better distance, and also populate
			// our "next hop" map with this edge. We'll also shave
			// off irrelevant edges by adding the sufficient
			// capacity of an edge to our relaxation condition.
			if tempDist < distance[v].dist &&
				edgeInfo.Capacity >= amt {

				distance[v] = nodeWithDist{
					dist: tempDist,
					node: edge.Node,
				}
				prev[v] = edgeWithPrev{
					edge: &ChannelHop{
						ChannelEdgePolicy: edge,
						Capacity:          edgeInfo.Capacity,
					},
					prevNode: bestNode.PubKey,
				}

				// Add this new node to our heap as we'd like
				// to further explore down this edge.
				heap.Push(&nodeHeap, distance[v])
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// If the target node isn't found in the prev hop map, then a path
	// doesn't exist, so we terminate in an error.
	if _, ok := prev[newVertex(target)]; !ok {
		return nil, ErrNoPathFound
	}

	// If the potential route if below the max hop limit, then we'll use
	// the prevHop map to unravel the path. We end up with a list of edges
	// in the reverse direction which we'll use to properly calculate the
	// timelock and fee values.
	pathEdges := make([]*ChannelHop, 0, len(prev))
	prevNode := newVertex(target)
	for prevNode != sourceVertex { // TODO(roasbeef): assumes no cycles
		// Add the current hop to the limit of path edges then walk
		// backwards from this hop via the prev pointer for this hop
		// within the prevHop map.
		pathEdges = append(pathEdges, prev[prevNode].edge)
		prev[prevNode].edge.Node.PubKey.Curve = nil

		prevNode = newVertex(prev[prevNode].prevNode)
	}

	// The route is invalid if it spans more than 20 hops. The current
	// Sphinx (onion routing) implementation can only encode up to 20 hops
	// as the entire packet is fixed size. If this route is more than 20
	// hops, then it's invalid.
	numEdges := len(pathEdges)
	if numEdges > HopLimit {
		return nil, ErrMaxHopsExceeded
	}

	// As our traversal of the prev map above walked backwards from the
	// target to the source in the route, we need to reverse it before
	// returning the final route.
	for i := 0; i < numEdges/2; i++ {
		pathEdges[i], pathEdges[numEdges-i-1] = pathEdges[numEdges-i-1], pathEdges[i]
	}

	return pathEdges, nil
}
}
