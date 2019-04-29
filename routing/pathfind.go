package routing

import (
	"container/heap"
	"math"

	"github.com/coreos/bbolt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// HopLimit is the maximum number hops that is permissible as a route.
	// Any potential paths found that lie above this limit will be rejected
	// with an error. This value is computed using the current fixed-size
	// packet length of the Sphinx construction.
	HopLimit = 20

	// infinity is used as a starting distance in our shortest path search.
	infinity = math.MaxInt64

	// RiskFactorBillionths controls the influence of time lock delta
	// of a channel on route selection. It is expressed as billionths
	// of msat per msat sent through the channel per time lock delta
	// block. See edgeWeight function below for more details.
	// The chosen value is based on the previous incorrect weight function
	// 1 + timelock + fee * fee. In this function, the fee penalty
	// diminishes the time lock penalty for all but the smallest amounts.
	// To not change the behaviour of path finding too drastically, a
	// relatively small value is chosen which is still big enough to give
	// some effect with smaller time lock values. The value may need
	// tweaking and/or be made configurable in the future.
	RiskFactorBillionths = 15
)

// pathFinder defines the interface of a path finding algorithm.
type pathFinder = func(g *graphParams, r *RestrictParams,
	source, target route.Vertex, amt lnwire.MilliSatoshi) (
	[]*channeldb.ChannelEdgePolicy, error)

// edgePolicyWithSource is a helper struct to keep track of the source node
// of a channel edge. ChannelEdgePolicy only contains to destination node
// of the edge.
type edgePolicyWithSource struct {
	sourceNode *channeldb.LightningNode
	edge       *channeldb.ChannelEdgePolicy
}

// computeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func computeFee(amt lnwire.MilliSatoshi,
	edge *channeldb.ChannelEdgePolicy) lnwire.MilliSatoshi {

	return edge.FeeBaseMSat + (amt*edge.FeeProportionalMillionths)/1000000
}

// isSamePath returns true if path1 and path2 travel through the exact same
// edges, and false otherwise.
func isSamePath(path1, path2 []*channeldb.ChannelEdgePolicy) bool {
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

// newRoute returns a fully valid route between the source and target that's
// capable of supporting a payment of `amtToSend` after fees are fully
// computed. If the route is too long, or the selected path cannot support the
// fully payment including fees, then a non-nil error is returned.
//
// NOTE: The passed slice of ChannelHops MUST be sorted in forward order: from
// the source to the target node of the path finding attempt.
func newRoute(amtToSend lnwire.MilliSatoshi, sourceVertex route.Vertex,
	pathEdges []*channeldb.ChannelEdgePolicy, currentHeight uint32,
	finalCLTVDelta uint16) (*route.Route, error) {

	var (
		hops []*route.Hop

		// totalTimeLock will accumulate the cumulative time lock
		// across the entire route. This value represents how long the
		// sender will need to wait in the *worst* case.
		totalTimeLock = currentHeight

		// nextIncomingAmount is the amount that will need to flow into
		// the *next* hop. Since we're going to be walking the route
		// backwards below, this next hop gets closer and closer to the
		// sender of the payment.
		nextIncomingAmount lnwire.MilliSatoshi
	)

	pathLength := len(pathEdges)
	for i := pathLength - 1; i >= 0; i-- {
		// Now we'll start to calculate the items within the per-hop
		// payload for the hop this edge is leading to.
		edge := pathEdges[i]

		// If this is the last hop, then the hop payload will contain
		// the exact amount. In BOLT #4: Onion Routing
		// Protocol / "Payload for the Last Node", this is detailed.
		amtToForward := amtToSend

		// Fee is not part of the hop payload, but only used for
		// reporting through RPC. Set to zero for the final hop.
		fee := lnwire.MilliSatoshi(0)

		// If the current hop isn't the last hop, then add enough funds
		// to pay for transit over the next link.
		if i != len(pathEdges)-1 {
			// The amount that the current hop needs to forward is
			// equal to the incoming amount of the next hop.
			amtToForward = nextIncomingAmount

			// The fee that needs to be paid to the current hop is
			// based on the amount that this hop needs to forward
			// and its policy for the outgoing channel. This policy
			// is stored as part of the incoming channel of
			// the next hop.
			fee = computeFee(amtToForward, pathEdges[i+1])
		}

		// If this is the last hop, then for verification purposes, the
		// value of the outgoing time-lock should be _exactly_ the
		// absolute time out they'd expect in the HTLC.
		var outgoingTimeLock uint32
		if i == len(pathEdges)-1 {
			// As this is the last hop, we'll use the specified
			// final CLTV delta value instead of the value from the
			// last link in the route.
			totalTimeLock += uint32(finalCLTVDelta)

			outgoingTimeLock = currentHeight + uint32(finalCLTVDelta)
		} else {
			// Next, increment the total timelock of the entire
			// route such that each hops time lock increases as we
			// walk backwards in the route, using the delta of the
			// previous hop.
			delta := uint32(pathEdges[i+1].TimeLockDelta)
			totalTimeLock += delta

			// Otherwise, the value of the outgoing time-lock will
			// be the value of the time-lock for the _outgoing_
			// HTLC, so we factor in their specified grace period
			// (time lock delta).
			outgoingTimeLock = totalTimeLock - delta
		}

		// Since we're traversing the path backwards atm, we prepend
		// each new hop such that, the final slice of hops will be in
		// the forwards order.
		currentHop := &route.Hop{
			PubKeyBytes:      edge.Node.PubKeyBytes,
			ChannelID:        edge.ChannelID,
			AmtToForward:     amtToForward,
			OutgoingTimeLock: outgoingTimeLock,
		}
		hops = append([]*route.Hop{currentHop}, hops...)

		// Finally, we update the amount that needs to flow into the
		// *next* hop, which is the amount this hop needs to forward,
		// accounting for the fee that it takes.
		nextIncomingAmount = amtToForward + fee
	}

	// With the base routing data expressed as hops, build the full route
	newRoute, err := route.NewRouteFromHops(
		nextIncomingAmount, totalTimeLock, route.Vertex(sourceVertex), hops,
	)
	if err != nil {
		return nil, err
	}

	return newRoute, nil
}

// edgeWeight computes the weight of an edge. This value is used when searching
// for the shortest path within the channel graph between two nodes. Weight is
// is the fee itself plus a time lock penalty added to it. This benefits
// channels with shorter time lock deltas and shorter (hops) routes in general.
// RiskFactor controls the influence of time lock on route selection. This is
// currently a fixed value, but might be configurable in the future.
func edgeWeight(lockedAmt lnwire.MilliSatoshi, fee lnwire.MilliSatoshi,
	timeLockDelta uint16) int64 {
	// timeLockPenalty is the penalty for the time lock delta of this channel.
	// It is controlled by RiskFactorBillionths and scales proportional
	// to the amount that will pass through channel. Rationale is that it if
	// a twice as large amount gets locked up, it is twice as bad.
	timeLockPenalty := int64(lockedAmt) * int64(timeLockDelta) *
		RiskFactorBillionths / 1000000000

	return int64(fee) + timeLockPenalty
}

// graphParams wraps the set of graph parameters passed to findPath.
type graphParams struct {
	// tx can be set to an existing db transaction. If not set, a new
	// transaction will be started.
	tx *bbolt.Tx

	// graph is the ChannelGraph to be used during path finding.
	graph *channeldb.ChannelGraph

	// additionalEdges is an optional set of edges that should be
	// considered during path finding, that is not already found in the
	// channel graph.
	additionalEdges map[route.Vertex][]*channeldb.ChannelEdgePolicy

	// bandwidthHints is an optional map from channels to bandwidths that
	// can be populated if the caller has a better estimate of the current
	// channel bandwidth than what is found in the graph. If set, it will
	// override the capacities and disabled flags found in the graph for
	// local channels when doing path finding. In particular, it should be
	// set to the current available sending bandwidth for active local
	// channels, and 0 for inactive channels.
	bandwidthHints map[uint64]lnwire.MilliSatoshi
}

// RestrictParams wraps the set of restrictions passed to findPath that the
// found path must adhere to.
type RestrictParams struct {
	// IgnoredNodes is an optional set of nodes that should be ignored if
	// encountered during path finding.
	IgnoredNodes map[route.Vertex]struct{}

	// IgnoredEdges is an optional set of edges that should be ignored if
	// encountered during path finding.
	IgnoredEdges map[EdgeLocator]struct{}

	// FeeLimit is a maximum fee amount allowed to be used on the path from
	// the source to the target.
	FeeLimit lnwire.MilliSatoshi

	// OutgoingChannelID is the channel that needs to be taken to the first
	// hop. If nil, any channel may be used.
	OutgoingChannelID *uint64

	// CltvLimit is the maximum time lock of the route excluding the final
	// ctlv. After path finding is complete, the caller needs to increase
	// all cltv expiry heights with the required final cltv delta.
	CltvLimit *uint32
}

// findPath attempts to find a path from the source node within the
// ChannelGraph to the target node that's capable of supporting a payment of
// `amt` value. The current approach implemented is modified version of
// Dijkstra's algorithm to find a single shortest path between the source node
// and the destination. The distance metric used for edges is related to the
// time-lock+fee costs along a particular edge. If a path is found, this
// function returns a slice of ChannelHop structs which encoded the chosen path
// from the target to the source. The search is performed backwards from
// destination node back to source. This is to properly accumulate fees
// that need to be paid along the path and accurately check the amount
// to forward at every node against the available bandwidth.
func findPath(g *graphParams, r *RestrictParams, source, target route.Vertex,
	amt lnwire.MilliSatoshi) ([]*channeldb.ChannelEdgePolicy, error) {

	var err error
	tx := g.tx
	if tx == nil {
		tx, err = g.graph.Database().Begin(false)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
	}

	// First we'll initialize an empty heap which'll help us to quickly
	// locate the next edge we should visit next during our graph
	// traversal.
	var nodeHeap distanceHeap

	// For each node in the graph, we create an entry in the distance map
	// for the node set with a distance of "infinity". graph.ForEachNode
	// also returns the source node, so there is no need to add the source
	// node explicitly.
	distance := make(map[route.Vertex]nodeWithDist)
	if err := g.graph.ForEachNode(tx, func(_ *bbolt.Tx,
		node *channeldb.LightningNode) error {
		// TODO(roasbeef): with larger graph can just use disk seeks
		// with a visited map
		distance[route.Vertex(node.PubKeyBytes)] = nodeWithDist{
			dist: infinity,
			node: node,
		}
		return nil
	}); err != nil {
		return nil, err
	}

	additionalEdgesWithSrc := make(map[route.Vertex][]*edgePolicyWithSource)
	for vertex, outgoingEdgePolicies := range g.additionalEdges {
		// We'll also include all the nodes found within the additional
		// edges that are not known to us yet in the distance map.
		node := &channeldb.LightningNode{PubKeyBytes: vertex}
		distance[vertex] = nodeWithDist{
			dist: infinity,
			node: node,
		}

		// Build reverse lookup to find incoming edges. Needed because
		// search is taken place from target to source.
		for _, outgoingEdgePolicy := range outgoingEdgePolicies {
			toVertex := outgoingEdgePolicy.Node.PubKeyBytes
			incomingEdgePolicy := &edgePolicyWithSource{
				sourceNode: node,
				edge:       outgoingEdgePolicy,
			}

			additionalEdgesWithSrc[toVertex] =
				append(additionalEdgesWithSrc[toVertex],
					incomingEdgePolicy)
		}
	}

	// We can't always assume that the end destination is publicly
	// advertised to the network and included in the graph.ForEachNode call
	// above, so we'll manually include the target node. The target node
	// charges no fee. Distance is set to 0, because this is the starting
	// point of the graph traversal. We are searching backwards to get the
	// fees first time right and correctly match channel bandwidth.
	targetNode := &channeldb.LightningNode{PubKeyBytes: target}
	distance[target] = nodeWithDist{
		dist:            0,
		node:            targetNode,
		amountToReceive: amt,
		fee:             0,
		incomingCltv:    0,
	}

	// We'll use this map as a series of "next" hop pointers. So to get
	// from `Vertex` to the target node, we'll take the edge that it's
	// mapped to within `next`.
	next := make(map[route.Vertex]*channeldb.ChannelEdgePolicy)

	ignoredEdges := r.IgnoredEdges
	if ignoredEdges == nil {
		ignoredEdges = make(map[EdgeLocator]struct{})
	}
	ignoredNodes := r.IgnoredNodes
	if ignoredNodes == nil {
		ignoredNodes = make(map[route.Vertex]struct{})
	}

	// processEdge is a helper closure that will be used to make sure edges
	// satisfy our specific requirements.
	processEdge := func(fromNode *channeldb.LightningNode,
		edge *channeldb.ChannelEdgePolicy,
		bandwidth lnwire.MilliSatoshi, toNode route.Vertex) {

		fromVertex := route.Vertex(fromNode.PubKeyBytes)

		// If this is not a local channel and it is disabled, we will
		// skip it.
		// TODO(halseth): also ignore disable flags for non-local
		// channels if bandwidth hint is set?
		isSourceChan := fromVertex == source

		edgeFlags := edge.ChannelFlags
		isDisabled := edgeFlags&lnwire.ChanUpdateDisabled != 0

		if !isSourceChan && isDisabled {
			return
		}

		// If we have an outgoing channel restriction and this is not
		// the specified channel, skip it.
		if isSourceChan && r.OutgoingChannelID != nil &&
			*r.OutgoingChannelID != edge.ChannelID {

			return
		}

		// If this vertex or edge has been black listed, then we'll
		// skip exploring this edge.
		if _, ok := ignoredNodes[fromVertex]; ok {
			return
		}

		locator := newEdgeLocator(edge)
		if _, ok := ignoredEdges[*locator]; ok {
			return
		}

		toNodeDist := distance[toNode]

		amountToSend := toNodeDist.amountToReceive

		// If the estimated bandwidth of the channel edge is not able
		// to carry the amount that needs to be send, return.
		if bandwidth < amountToSend {
			return
		}

		// If the amountToSend is less than the minimum required
		// amount, return.
		if amountToSend < edge.MinHTLC {
			return
		}

		// If this edge was constructed from a hop hint, we won't have access to
		// its max HTLC. Therefore, only consider discarding this edge here if
		// the field is set.
		if edge.MaxHTLC != 0 && edge.MaxHTLC < amountToSend {
			return
		}

		// Compute fee that fromNode is charging. It is based on the
		// amount that needs to be sent to the next node in the route.
		//
		// Source node has no predecessor to pay a fee. Therefore set
		// fee to zero, because it should not be included in the fee
		// limit check and edge weight.
		//
		// Also determine the time lock delta that will be added to the
		// route if fromNode is selected. If fromNode is the source
		// node, no additional timelock is required.
		var fee lnwire.MilliSatoshi
		var timeLockDelta uint16
		if fromVertex != source {
			fee = computeFee(amountToSend, edge)
			timeLockDelta = edge.TimeLockDelta
		}

		incomingCltv := toNodeDist.incomingCltv +
			uint32(timeLockDelta)

		// Check that we have cltv limit and that we are within it.
		if r.CltvLimit != nil && incomingCltv > *r.CltvLimit {
			return
		}

		// amountToReceive is the amount that the node that is added to
		// the distance map needs to receive from a (to be found)
		// previous node in the route. That previous node will need to
		// pay the amount that this node forwards plus the fee it
		// charges.
		amountToReceive := amountToSend + fee

		// Check if accumulated fees would exceed fee limit when this
		// node would be added to the path.
		totalFee := amountToReceive - amt
		if totalFee > r.FeeLimit {
			return
		}

		// By adding fromNode in the route, there will be an extra
		// weight composed of the fee that this node will charge and
		// the amount that will be locked for timeLockDelta blocks in
		// the HTLC that is handed out to fromNode.
		weight := edgeWeight(amountToReceive, fee, timeLockDelta)

		// Compute the tentative distance to this new channel/edge
		// which is the distance from our toNode to the target node
		// plus the weight of this edge.
		tempDist := toNodeDist.dist + weight

		// If this new tentative distance is not better than the current
		// best known distance to this node, return.
		if tempDist >= distance[fromVertex].dist {
			return
		}

		// Every edge should have a positive time lock delta. If we
		// encounter a zero delta, log a warning line.
		if edge.TimeLockDelta == 0 {
			log.Warnf("Channel %v has zero cltv delta",
				edge.ChannelID)
		}

		// All conditions are met and this new tentative distance is
		// better than the current best known distance to this node.
		// The new better distance is recorded, and also our "next hop"
		// map is populated with this edge.
		distance[fromVertex] = nodeWithDist{
			dist:            tempDist,
			node:            fromNode,
			amountToReceive: amountToReceive,
			fee:             fee,
			incomingCltv:    incomingCltv,
		}

		next[fromVertex] = edge

		// Add this new node to our heap as we'd like to further
		// explore backwards through this edge.
		heap.Push(&nodeHeap, distance[fromVertex])
	}

	// TODO(roasbeef): also add path caching
	//  * similar to route caching, but doesn't factor in the amount

	// To start, our target node will the sole item within our distance
	// heap.
	heap.Push(&nodeHeap, distance[target])

	for nodeHeap.Len() != 0 {
		// Fetch the node within the smallest distance from our source
		// from the heap.
		partialPath := heap.Pop(&nodeHeap).(nodeWithDist)
		bestNode := partialPath.node

		// If we've reached our source (or we don't have any incoming
		// edges), then we're done here and can exit the graph
		// traversal early.
		if bestNode.PubKeyBytes == source {
			break
		}

		// Now that we've found the next potential step to take we'll
		// examine all the incoming edges (channels) from this node to
		// further our graph traversal.
		pivot := route.Vertex(bestNode.PubKeyBytes)
		err := bestNode.ForEachChannel(tx, func(tx *bbolt.Tx,
			edgeInfo *channeldb.ChannelEdgeInfo,
			_, inEdge *channeldb.ChannelEdgePolicy) error {

			// If there is no edge policy for this candidate
			// node, skip. Note that we are searching backwards
			// so this node would have come prior to the pivot
			// node in the route.
			if inEdge == nil {
				return nil
			}

			// We'll query the lower layer to see if we can obtain
			// any more up to date information concerning the
			// bandwidth of this edge.
			edgeBandwidth, ok := g.bandwidthHints[edgeInfo.ChannelID]
			if !ok {
				// If we don't have a hint for this edge, then
				// we'll just use the known Capacity/MaxHTLC as
				// the available bandwidth. It's possible for
				// the capacity to be unknown when operating
				// under a light client.
				edgeBandwidth = inEdge.MaxHTLC
				if edgeBandwidth == 0 {
					edgeBandwidth = lnwire.NewMSatFromSatoshis(
						edgeInfo.Capacity,
					)
				}
			}

			// Before we can process the edge, we'll need to fetch
			// the node on the _other_ end of this channel as we
			// may later need to iterate over the incoming edges of
			// this node if we explore it further.
			channelSource, err := edgeInfo.FetchOtherNode(
				tx, pivot[:],
			)
			if err != nil {
				return err
			}

			// Check if this candidate node is better than what we
			// already have.
			processEdge(channelSource, inEdge, edgeBandwidth, pivot)
			return nil
		})
		if err != nil {
			return nil, err
		}

		// Then, we'll examine all the additional edges from the node
		// we're currently visiting. Since we don't know the capacity
		// of the private channel, we'll assume it was selected as a
		// routing hint due to having enough capacity for the payment
		// and use the payment amount as its capacity.
		bandWidth := partialPath.amountToReceive
		for _, reverseEdge := range additionalEdgesWithSrc[bestNode.PubKeyBytes] {
			processEdge(reverseEdge.sourceNode, reverseEdge.edge,
				bandWidth, pivot)
		}
	}

	// If the source node isn't found in the next hop map, then a path
	// doesn't exist, so we terminate in an error.
	if _, ok := next[source]; !ok {
		return nil, newErrf(ErrNoPathFound, "unable to find a path to "+
			"destination")
	}

	// Use the nextHop map to unravel the forward path from source to
	// target.
	pathEdges := make([]*channeldb.ChannelEdgePolicy, 0, len(next))
	currentNode := source
	for currentNode != target { // TODO(roasbeef): assumes no cycles
		// Determine the next hop forward using the next map.
		nextNode := next[currentNode]

		// Add the next hop to the list of path edges.
		pathEdges = append(pathEdges, nextNode)

		// Advance current node.
		currentNode = route.Vertex(nextNode.Node.PubKeyBytes)
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

	return pathEdges, nil
}

// findPaths implements a k-shortest paths algorithm to find all the reachable
// paths between the passed source and target. The algorithm will continue to
// traverse the graph until all possible candidate paths have been depleted.
// This function implements a modified version of Yen's. To find each path
// itself, we utilize our modified version of Dijkstra's found above. When
// examining possible spur and root paths, rather than removing edges or
// Vertexes from the graph, we instead utilize a Vertex+edge black-list that
// will be ignored by our modified Dijkstra's algorithm. With this approach, we
// make our inner path finding algorithm aware of our k-shortest paths
// algorithm, rather than attempting to use an unmodified path finding
// algorithm in a block box manner.
func findPaths(tx *bbolt.Tx, graph *channeldb.ChannelGraph,
	source, target route.Vertex, amt lnwire.MilliSatoshi,
	restrictions *RestrictParams, numPaths uint32,
	bandwidthHints map[uint64]lnwire.MilliSatoshi) (
	[][]*channeldb.ChannelEdgePolicy, error) {

	// TODO(roasbeef): modifying ordering within heap to eliminate final
	// sorting step?
	var (
		shortestPaths  [][]*channeldb.ChannelEdgePolicy
		candidatePaths pathHeap
	)

	// First we'll find a single shortest path from the source (our
	// selfNode) to the target destination that's capable of carrying amt
	// satoshis along the path before fees are calculated.
	startingPath, err := findPath(
		&graphParams{
			tx:             tx,
			graph:          graph,
			bandwidthHints: bandwidthHints,
		},
		restrictions, source, target, amt,
	)
	if err != nil {
		log.Errorf("Unable to find path: %v", err)
		return nil, err
	}

	// Manually insert a "self" edge emanating from ourselves. This
	// self-edge is required in order for the path finding algorithm to
	// function properly.
	firstPath := make([]*channeldb.ChannelEdgePolicy, 0, len(startingPath)+1)
	firstPath = append(firstPath, &channeldb.ChannelEdgePolicy{
		Node: &channeldb.LightningNode{PubKeyBytes: source},
	})
	firstPath = append(firstPath, startingPath...)

	shortestPaths = append(shortestPaths, firstPath)

	// While we still have candidate paths to explore we'll keep exploring
	// the sub-graphs created to find the next k-th shortest path.
	for k := uint32(1); k < numPaths; k++ {
		prevShortest := shortestPaths[k-1]

		// We'll examine each edge in the previous iteration's shortest
		// path in order to find path deviations from each node in the
		// path.
		for i := 0; i < len(prevShortest)-1; i++ {
			// These two maps will mark the edges and Vertexes
			// we'll exclude from the next path finding attempt.
			// These are required to ensure the paths are unique
			// and loopless.
			ignoredEdges := make(map[EdgeLocator]struct{})
			ignoredVertexes := make(map[route.Vertex]struct{})

			for e := range restrictions.IgnoredEdges {
				ignoredEdges[e] = struct{}{}
			}
			for n := range restrictions.IgnoredNodes {
				ignoredVertexes[n] = struct{}{}
			}

			// Our spur node is the i-th node in the prior shortest
			// path, and our root path will be all nodes in the
			// path leading up to our spurNode.
			spurNode := prevShortest[i].Node
			rootPath := prevShortest[:i+1]

			// Before we kickoff our next path finding iteration,
			// we'll find all the edges we need to ignore in this
			// next round. This ensures that we create a new unique
			// path.
			for _, path := range shortestPaths {
				// If our current rootPath is a prefix of this
				// shortest path, then we'll remove the edge
				// directly _after_ our spur node from the
				// graph so we don't repeat paths.
				if len(path) > i+1 &&
					isSamePath(rootPath, path[:i+1]) {

					locator := newEdgeLocator(path[i+1])
					ignoredEdges[*locator] = struct{}{}
				}
			}

			// Next we'll remove all entries in the root path that
			// aren't the current spur node from the graph. This
			// ensures we don't create a path with loops.
			for _, hop := range rootPath {
				node := hop.Node.PubKeyBytes
				if node == spurNode.PubKeyBytes {
					continue
				}

				ignoredVertexes[route.Vertex(node)] = struct{}{}
			}

			// With the edges that are part of our root path, and
			// the Vertexes (other than the spur path) within the
			// root path removed, we'll attempt to find another
			// shortest path from the spur node to the destination.
			//
			// TODO: Fee limit passed to spur path finding isn't
			// correct, because it doesn't take into account the
			// fees already paid on the root path.
			//
			// TODO: Outgoing channel restriction isn't obeyed for
			// spur paths.
			spurRestrictions := &RestrictParams{
				IgnoredEdges: ignoredEdges,
				IgnoredNodes: ignoredVertexes,
				FeeLimit:     restrictions.FeeLimit,
			}

			spurPath, err := findPath(
				&graphParams{
					tx:             tx,
					graph:          graph,
					bandwidthHints: bandwidthHints,
				},
				spurRestrictions, spurNode.PubKeyBytes,
				target, amt,
			)

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
				hops: make([]*channeldb.ChannelEdgePolicy, 0, newPathLen),
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
