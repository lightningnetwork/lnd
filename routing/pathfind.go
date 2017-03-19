package routing

import (
	"math"

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

// newRoute returns a fully valid route between the source and target that's
// capable of supporting a payment of `amtToSend` after fees are fully
// computed. IF the route is too long, or the selected path cannot support the
// fully payment including fees, then a non-nil error is returned. prevHop maps
// a vertex to the channel required to get to it.
func newRoute(amtToSend btcutil.Amount, source, target vertex,
	prevHop map[vertex]edgeWithPrev) (*Route, error) {

	// If the potential route if below the max hop limit, then we'll use
	// the prevHop map to unravel the path. We end up with a list of edges
	// in the reverse direction which we'll use to properly calculate the
	// timelock and fee values.
	pathEdges := make([]*ChannelHop, 0, len(prevHop))
	prev := target
	for prev != source { // TODO(roasbeef): assumes no cycles
		// Add the current hop to the limit of path edges then walk
		// backwards from this hop via the prev pointer for this hop
		// within the prevHop map.
		pathEdges = append(pathEdges, prevHop[prev].edge)
		prev = newVertex(prevHop[prev].prevNode)
	}

	// The route is invalid if it spans more than 20 hops. The current
	// Sphinx (onion routing) implementation can only encode up to 20 hops
	// as the entire packet is fixed size. If this route is more than 20 hops,
	// then it's invalid.
	if len(pathEdges) > HopLimit {
		return nil, ErrMaxHopsExceeded
	}

	route := &Route{
		Hops: make([]*Hop, len(pathEdges)),
	}

	// The running amount is the total amount of satoshis required at this
	// point in the route. We start this value at the amount we want to
	// send to the destination. This value will then get successively
	// larger as we compute the fees going backwards.
	runningAmt := amtToSend
	pathLength := len(pathEdges)
	for i, edge := range pathEdges {
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

		// Finally, as we're currently talking the route backwards, we
		// reverse the index in order to place this hop at the proper
		// spot in the forward direction of the route.
		route.Hops[pathLength-1-i] = nextHop
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

// findRoute attempts to find a path from the source node within the
// ChannelGraph to the target node that's capable of supporting a payment of
// `amt` value. The current approach is used a multiple pass path finding
// algorithm. First we employ a modified version of Dijkstra's algorithm to
// find a potential set of shortest paths, the distance metric is related to
// the time-lock+fee along the route. Once we have a set of candidate routes,
// we calculate the required fee and time lock values running backwards along
// the route. The route that's selected is the one with the lowest total fee.
//
// TODO(roasbeef): make member, add caching
//  * add k-path
func findRoute(graph *channeldb.ChannelGraph, target *btcec.PublicKey,
	amt btcutil.Amount) (*Route, error) {

	// First initialize empty list of all the node that we've yet to
	// visited.
	// TODO(roasbeef): make into incremental fibonacci heap rather than
	// loading all into memory.
	var unvisited []*channeldb.LightningNode

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

		unvisited = append(unvisited, node)
		return nil
	}); err != nil {
		return nil, err
	}

	// Next we obtain the source node from the graph, and initialize it
	// with a distance of 0. This indicates our starting point in the graph
	// traversal.
	sourceNode, err := graph.SourceNode()
	if err != nil {
		return nil, err
	}
	sourceVertex := newVertex(sourceNode.PubKey)
	distance[sourceVertex] = nodeWithDist{
		dist: 0,
		node: sourceNode,
	}

	// We'll use this map as a series of "previous" hop pointers. So to get
	// to `vertex` we'll take the edge that it's mapped to within `prev`.
	prev := make(map[vertex]edgeWithPrev)

	for len(unvisited) != 0 {
		var bestNode *channeldb.LightningNode
		smallestDist := infinity

		// First we examine our list of unvisited nodes, for the most
		// optimal vertex to examine next.
		for i, node := range unvisited {
			// The "best" node to visit next is node with the
			// smallest distance from the source of all the
			// unvisited nodes.
			v := newVertex(node.PubKey)
			if nodeInfo := distance[v]; nodeInfo.dist < smallestDist {
				smallestDist = nodeInfo.dist
				bestNode = nodeInfo.node

				// Since we're going to visit this node, we can
				// remove it from the set of unvisited nodes.
				copy(unvisited[i:], unvisited[i+1:])
				unvisited[len(unvisited)-1] = nil // Avoid GC leak.
				unvisited = unvisited[:len(unvisited)-1]

				break
			}
		}

		// If we've reached our target (or we don't have any outgoing
		// edges), then we're done here and can exit the graph
		// traversal early.
		if bestNode == nil || bestNode.PubKey.IsEqual(target) {
			break
		}

		// Now that we've found the next potential step to take we'll
		// examine all the outgoing edge (channels) from this node to
		// further our graph traversal.
		pivot := newVertex(bestNode.PubKey)
		err := bestNode.ForEachChannel(nil, func(edgeInfo *channeldb.ChannelEdgeInfo,
			edge *channeldb.ChannelEdgePolicy) error {

			// Compute the tentative distance to this new
			// channel/edge which is the distance to our current
			// pivot node plus the weight of this edge.
			tempDist := distance[pivot].dist + edgeWeight(edge)

			// If this new tentative distance is better than the
			// current best known distance to this node, then we
			// record the new better distance, and also populate
			// our "next hop" map with this edge.
			// TODO(roasbeef): add capacity to relaxation criteria?
			//  * also add min payment?
			v := newVertex(edge.Node.PubKey)
			if tempDist < distance[v].dist {
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

	// Otherwise, we construct a new route which calculate the relevant
	// total fees and proper time lock values for each hop.
	targetVerex := newVertex(target)
	return newRoute(amt, sourceVertex, targetVerex, prev)
}
