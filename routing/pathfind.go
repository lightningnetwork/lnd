package routing

import (
	"encoding/binary"
	"fmt"
	"math"

	"container/heap"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
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

	// OutgoingTimeLock is the timelock value that should be used when
	// crafting the _outgoing_ HTLC from this hop.
	OutgoingTimeLock uint32

	// AmtToForward is the amount that this hop will forward to the next
	// hop. This value is less than the value that the incoming HTLC
	// carries as a fee will be subtracted by the hop.
	AmtToForward lnwire.MilliSatoshi

	// Fee is the total fee that this hop will subtract from the incoming
	// payment, this difference nets the hop fees for forwarding the
	// payment.
	Fee lnwire.MilliSatoshi
}

// computeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func computeFee(amt lnwire.MilliSatoshi, edge *ChannelHop) lnwire.MilliSatoshi {
	return edge.FeeBaseMSat + (amt*edge.FeeProportionalMillionths)/1000000
}

// isSamePath returns true if path1 and path2 travel through the exact same
// edges, and false otherwise.
func isSamePath(path1, path2 []*ChannelHop) bool {
	if len(path1) != len(path2) {
		return false
	}

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
	TotalFees lnwire.MilliSatoshi

	// TotalAmount is the total amount of funds required to complete a
	// payment over this route. This value includes the cumulative fees at
	// each hop. As a result, the HTLC extended to the first-hop in the
	// route will need to have at least this many satoshis, otherwise the
	// route will fail at an intermediate node due to an insufficient
	// amount of fees.
	TotalAmount lnwire.MilliSatoshi

	// Hops contains details concerning the specific forwarding details at
	// each hop.
	Hops []*Hop
}

// ToHopPayloads converts a complete route into the series of per-hop payloads
// that is to be encoded within each HTLC using an opaque Sphinx packet.
func (r *Route) ToHopPayloads() []sphinx.HopData {
	hopPayloads := make([]sphinx.HopData, len(r.Hops))

	// For each hop encoded within the route, we'll convert the hop struct
	// to the matching per-hop payload struct as used by the sphinx
	// package.
	for i, hop := range r.Hops {
		hopPayloads[i] = sphinx.HopData{
			// TODO(roasbeef): properly set realm, make sphinx type
			// an enum actually?
			Realm:         0,
			ForwardAmount: uint64(hop.AmtToForward),
			OutgoingCltv:  hop.OutgoingTimeLock,
		}

		// As a base case, the next hop is set to all zeroes in order
		// to indicate that the "last hop" as no further hops after it.
		nextHop := uint64(0)

		// If we aren't on the last hop, then we set the "next address"
		// field to be the channel that directly follows it.
		if i != len(r.Hops)-1 {
			nextHop = r.Hops[i+1].Channel.ChannelID
		}

		binary.BigEndian.PutUint64(hopPayloads[i].NextAddress[:],
			nextHop)
	}

	return hopPayloads
}

// sortableRoutes is a slice of routes that can be sorted. Routes are typically
// sorted according to their total cumulative fee within the route. In the case
// that two routes require and identical amount of fees, then the total
// time-lock will be used as the tie breaker.
type sortableRoutes []*Route

// Len returns the number of routes in the collection.
//
// NOTE: This is part of the sort.Interface implementation.
func (s sortableRoutes) Len() int {
	return len(s)
}

// Less reports whether the route with index i should sort before the route
// with index j. To make this decision we first check if the total fees
// required for both routes are equal. If so, then we'll let the total time
// lock be the tie breaker. Otherwise, we'll put the route with the lowest
// total fees first.
//
// NOTE: This is part of the sort.Interface implementation.
func (s sortableRoutes) Less(i, j int) bool {
	if s[i].TotalFees == s[j].TotalFees {
		return s[i].TotalTimeLock < s[j].TotalTimeLock
	}

	return s[i].TotalFees < s[j].TotalFees
}

// Swap swaps the elements with indexes i and j.
//
// NOTE: This is part of the sort.Interface implementation.
func (s sortableRoutes) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// newRoute returns a fully valid route between the source and target that's
// capable of supporting a payment of `amtToSend` after fees are fully
// computed. If the route is too long, or the selected path cannot support the
// fully payment including fees, then a non-nil error is returned.
//
// NOTE: The passed slice of ChannelHops MUST be sorted in forward order: from
// the source to the target node of the path finding attempt.
func newRoute(amtToSend lnwire.MilliSatoshi, pathEdges []*ChannelHop,
	currentHeight uint32) (*Route, error) {

	// First, we'll create a new empty route with enough hops to match the
	// amount of path edges. We set the TotalTimeLock to the current block
	// height, as this is the basis that all of the time locks will be
	// calculated from.
	route := &Route{
		Hops:          make([]*Hop, len(pathEdges)),
		TotalTimeLock: currentHeight,
	}

	// TODO(roasbeef): need to do sanity check to ensure we don't make a
	// "dust" payment: over x% of money sending to fees

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
			Channel:      edge,
			AmtToForward: runningAmt,
			Fee:          computeFee(runningAmt, edge),
		}
		edge.Node.PubKey.Curve = nil

		// As a sanity check, we ensure that the selected channel has
		// enough capacity to forward the required amount which
		// includes the fee dictated at each hop.
		if nextHop.AmtToForward.ToSatoshis() > nextHop.Channel.Capacity {
			err := fmt.Sprintf("channel graph has insufficient "+
				"capacity for the payment: need %v, have %v",
				nextHop.AmtToForward.ToSatoshis(),
				nextHop.Channel.Capacity)

			return nil, newErrf(ErrInsufficientCapacity, err)
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

		// Next, increment the total timelock of the entire route such
		// that each hops time lock increases as we walk backwards in
		// the route, using the delta of the previous hop.
		route.TotalTimeLock += uint32(edge.TimeLockDelta)

		// If this is the last hop, then for verification purposes, the
		// value of the outgoing time-lock should be _exactly_ the
		// absolute time out they'd expect in the HTLC.
		if i == len(pathEdges)-1 {
			nextHop.OutgoingTimeLock = currentHeight + uint32(edge.TimeLockDelta)
		} else {
			// Otherwise, the value of the outgoing time-lock will
			// be the value of the time-lock for the _outgoing_
			// HTLC, so we factor in their specified grace period
			// (time lock delta).
			nextHop.OutgoingTimeLock = route.TotalTimeLock -
				uint32(edge.TimeLockDelta)
		}

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
	ignoredEdges map[uint64]struct{}, amt lnwire.MilliSatoshi) ([]*ChannelHop, error) {

	// First we'll initialize an empty heap which'll help us to quickly
	// locate the next edge we should visit next during our graph
	// traversal.
	var nodeHeap distanceHeap

	// For each node/vertex the graph we create an entry in the distance
	// map for the node set with a distance of "infinity".  We also mark
	// add the node to our set of unvisited nodes.
	distance := make(map[vertex]nodeWithDist)
	if err := graph.ForEachNode(nil, func(_ *bolt.Tx, node *channeldb.LightningNode) error {
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
		err := bestNode.ForEachChannel(nil, func(tx *bolt.Tx,
			edgeInfo *channeldb.ChannelEdgeInfo,
			outEdge, inEdge *channeldb.ChannelEdgePolicy) error {

			v := newVertex(outEdge.Node.PubKey)

			// TODO(roasbeef): skip if disabled

			// If this vertex or edge has been black listed, then
			// we'll skip exploring this edge during this
			// iteration.
			if _, ok := ignoredNodes[v]; ok {
				return nil
			}
			if _, ok := ignoredEdges[outEdge.ChannelID]; ok {
				return nil
			}
			if inEdge == nil {
				return nil
			}

			// Compute the tentative distance to this new
			// channel/edge which is the distance to our current
			// pivot node plus the weight of this edge.
			tempDist := distance[pivot].dist + edgeWeight(inEdge)

			// If this new tentative distance is better than the
			// current best known distance to this node, then we
			// record the new better distance, and also populate
			// our "next hop" map with this edge. We'll also shave
			// off irrelevant edges by adding the sufficient
			// capacity of an edge to our relaxation condition.
			if tempDist < distance[v].dist &&
				edgeInfo.Capacity >= amt.ToSatoshis() {

				// TODO(roasbeef): need to also account
				// for min HTLC

				distance[v] = nodeWithDist{
					dist: tempDist,
					node: outEdge.Node,
				}
				prev[v] = edgeWithPrev{
					// We'll use the *incoming* edge here
					// as we need to use the routing policy
					// specified by the node this channel
					// connects to.
					edge: &ChannelHop{
						ChannelEdgePolicy: inEdge,
						Capacity:          edgeInfo.Capacity,
					},
					prevNode: bestNode.PubKey,
				}

				// In order for the path unwinding to work
				// properly, we'll ensure that this edge
				// properly points to the outgoing node.
				//
				// TODO(roasbeef): revisit, possibly switch db
				// format?
				prev[v].edge.Node = outEdge.Node

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
		return nil, newErrf(ErrNoPathFound, "unable to find a path to "+
			"destination")
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
		return nil, newErr(ErrMaxHopsExceeded, "potential path has "+
			"too many hops")
	}

	// As our traversal of the prev map above walked backwards from the
	// target to the source in the route, we need to reverse it before
	// returning the final route.
	for i := 0; i < numEdges/2; i++ {
		pathEdges[i], pathEdges[numEdges-i-1] = pathEdges[numEdges-i-1], pathEdges[i]
	}

	return pathEdges, nil
}

// findPaths implements a k-shortest paths algorithm to find all the reachable
// paths between the passed source and target. The algorithm will continue to
// traverse the graph until all possible candidate paths have been depleted.
// This function implements a modified version of Yen's. To find each path
// itself, we utilize our modified version of Dijkstra's found above. When
// examining possible spur and root paths, rather than removing edges or
// vertexes from the graph, we instead utilize a vertex+edge black-list that
// will be ignored by our modified Dijkstra's algorithm. With this approach, we
// make our inner path finding algorithm aware of our k-shortest paths
// algorithm, rather than attempting to use an unmodified path finding
// algorithm in a block box manner.
func findPaths(graph *channeldb.ChannelGraph, source *channeldb.LightningNode,
	target *btcec.PublicKey, amt lnwire.MilliSatoshi) ([][]*ChannelHop, error) {

	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[vertex]struct{})

	// TODO(roasbeef): modifying ordering within heap to eliminate final
	// sorting step?
	var (
		shortestPaths  [][]*ChannelHop
		candidatePaths pathHeap
	)

	// First we'll find a single shortest path from the source (our
	// selfNode) to the target destination that's capable of carrying amt
	// satoshis along the path before fees are calculated.
	startingPath, err := findPath(graph, source, target,
		ignoredVertexes, ignoredEdges, amt)
	if err != nil {
		log.Errorf("Unable to find path: %v", err)
		return nil, err
	}

	// Manually insert a "self" edge emanating from ourselves. This
	// self-edge is required in order for the path finding algorithm to
	// function properly.
	firstPath := make([]*ChannelHop, 0, len(startingPath)+1)
	firstPath = append(firstPath, &ChannelHop{
		ChannelEdgePolicy: &channeldb.ChannelEdgePolicy{
			Node: source,
		},
	})
	firstPath = append(firstPath, startingPath...)

	shortestPaths = append(shortestPaths, firstPath)

	source.PubKey.Curve = nil

	// While we still have candidate paths to explore we'll keep exploring
	// the sub-graphs created to find the next k-th shortest path.
	for k := 1; k < 100; k++ {
		prevShortest := shortestPaths[k-1]

		// We'll examine each edge in the previous iteration's shortest
		// path in order to find path deviations from each node in the
		// path.
		for i := 0; i < len(prevShortest)-1; i++ {
			// These two maps will mark the edges and vertexes
			// we'll exclude from the next path finding attempt.
			// These are required to ensure the paths are unique
			// and loopless.
			ignoredEdges = make(map[uint64]struct{})
			ignoredVertexes = make(map[vertex]struct{})

			// Our spur node is the i-th node in the prior shortest
			// path, and our root path will be all nodes in the
			// path leading up to our spurNode.
			spurNode := prevShortest[i].Node
			rootPath := prevShortest[:i+1]

			// Before we kickoff our next path finding iteration,
			// we'll find all the edges we need to ignore in this
			// next round.
			for _, path := range shortestPaths {
				// If our current rootPath is a prefix of this
				// shortest path, then we'll remove the edge
				// directly _after_ our spur node from the
				// graph so we don't repeat paths.
				if len(path) > i+1 && isSamePath(rootPath, path[:i+1]) {
					ignoredEdges[path[i+1].ChannelID] = struct{}{}
				}
			}

			// Next we'll remove all entries in the root path that
			// aren't the current spur node from the graph.
			for _, hop := range rootPath {
				node := hop.Node.PubKey
				if node.IsEqual(spurNode.PubKey) {
					continue
				}

				ignoredVertexes[newVertex(node)] = struct{}{}
			}

			// With the edges that are part of our root path, and
			// the vertexes (other than the spur path) within the
			// root path removed, we'll attempt to find another
			// shortest path from the spur node to the destination.
			spurPath, err := findPath(graph, spurNode, target,
				ignoredVertexes, ignoredEdges, amt)

			// If we weren't able to find a path, we'll continue to
			// the next round.
			if IsError(err, ErrNoPathFound) {
				continue
			} else if err != nil {
				return nil, err
			}

			// Create the new combined path by concatenating the
			// rootPath to the spurPath.
			newPathLen := len(rootPath) + len(spurPath)
			newPath := path{
				hops: make([]*ChannelHop, 0, newPathLen),
				dist: newPathLen,
			}
			newPath.hops = append(newPath.hops, rootPath...)
			newPath.hops = append(newPath.hops, spurPath...)

			// TODO(roasbeef): add and consult path finger print

			// We'll now add this newPath to the heap of candidate
			// shortest paths.
			heap.Push(&candidatePaths, newPath)
		}

		// If our min-heap of candidate paths is empty, then we can
		// exit early.
		if candidatePaths.Len() == 0 {
			break
		}

		// To conclude this latest iteration, we'll take the shortest
		// path in our set of candidate paths and add it to our
		// shortestPaths list as the *next* shortest path.
		nextShortestPath := heap.Pop(&candidatePaths).(path).hops
		shortestPaths = append(shortestPaths, nextShortestPath)
	}

	return shortestPaths, nil
}
