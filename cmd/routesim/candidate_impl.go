package main

// This file is the CANDIDATE SLOT for evolved routing algorithms. During
// optimization, the entire file is replaced (via go build -overlay) with a
// generated implementation. The contract is a single constructor:
//
//	newCandidateRouter(view, source, localBalances, spec)
//
// returning a routing.SimRouter. The router sees only the public gossip
// graph, its own channel balances and per-attempt feedback — the same
// information a real Lightning sender has. The in-tree implementation below
// is the seed algorithm: a deliberately simple fee-optimizing Dijkstra with
// failure blacklisting and halving-based MPP splitting.

import (
	"container/heap"
	"context"
	"errors"
	"fmt"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

// candidateEdge is one directed edge of the public graph: a channel from
// one node to another, with the policy the sending node announced.
type candidateEdge struct {
	chanID   uint64
	from, to route.Vertex
	capacity lnwire.MilliSatoshi

	baseFeeMsat   lnwire.MilliSatoshi
	feeRatePPM    lnwire.MilliSatoshi
	timeLockDelta uint16
	minHTLC       lnwire.MilliSatoshi
	maxHTLC       lnwire.MilliSatoshi
}

// fee returns the fee the sending node charges to forward amt over this
// edge.
func (e *candidateEdge) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return e.baseFeeMsat + amt*e.feeRatePPM/1_000_000
}

// usable reports whether the edge can carry the given amount per its
// announced policy.
func (e *candidateEdge) usable(amt lnwire.MilliSatoshi) bool {
	if amt < e.minHTLC {
		return false
	}
	if e.maxHTLC != 0 && amt > e.maxHTLC {
		return false
	}
	// The public capacity is a hard upper bound on what can flow.
	return amt <= e.capacity
}

// candidateRouter is the seed algorithm: cheapest-path routing with a
// failure blacklist and amount halving when no route is found.
type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	// incomingEdges maps a node to the directed edges arriving at it,
	// the natural shape for backward Dijkstra.
	incomingEdges map[route.Vertex][]*candidateEdge

	// localBalances is the exact outbound liquidity of our own channels.
	localBalances map[uint64]lnwire.MilliSatoshi

	// failedAmt records, per directed channel, the lowest amount that
	// failed with a liquidity error; routes are built to stay below it.
	failedAmt map[uint64]lnwire.MilliSatoshi

	// shardAmt is the current shard size for MPP splitting.
	shardAmt lnwire.MilliSatoshi

	// partsUsed counts the successful shards so far.
	partsUsed uint32

	// pending maps in-flight attempt ids to their routes.
	pending map[uint64]*route.Route
}

// newCandidateRouter builds the router for one payment. This signature is
// the stable contract between the harness and generated candidates.
func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	router := &candidateRouter{
		source:        source,
		spec:          spec,
		incomingEdges: make(map[route.Vertex][]*candidateEdge),
		localBalances: localBalances,
		failedAmt:     make(map[uint64]lnwire.MilliSatoshi),
		shardAmt:      spec.Amount,
		pending:       make(map[uint64]*route.Route),
	}

	// Build the adjacency list from gossip. Iterating a node's channels
	// yields, per channel, the policy the OTHER node announced toward us
	// (InPolicy). That is exactly the policy governing the directed edge
	// other -> node, so we record the reversed edge at each visit.
	ctx := context.Background()
	seen := make(map[route.Vertex]bool)
	queue := []route.Vertex{source}
	seen[source] = true

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		err := view.ForEachNodeDirectedChannel(ctx, node,
			func(ch *graphdb.DirectedChannel) error {
				if !seen[ch.OtherNode] {
					seen[ch.OtherNode] = true
					queue = append(queue, ch.OtherNode)
				}

				pol := ch.InPolicy
				if pol == nil || pol.IsDisabled {
					return nil
				}

				edge := &candidateEdge{
					chanID: ch.ChannelID,
					from:   ch.OtherNode,
					to:     node,
					capacity: lnwire.NewMSatFromSatoshis(
						ch.Capacity,
					),
					baseFeeMsat: pol.FeeBaseMSat,
					feeRatePPM: pol.
						FeeProportionalMillionths,
					timeLockDelta: pol.TimeLockDelta,
					minHTLC:       pol.MinHTLC,
				}
				if pol.HasMaxHTLC {
					edge.maxHTLC = pol.MaxHTLC
				}

				router.incomingEdges[edge.to] = append(
					router.incomingEdges[edge.to], edge,
				)

				return nil
			}, func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	return router, nil
}

// dijkstraItem is a priority queue entry.
type dijkstraItem struct {
	node route.Vertex
	cost lnwire.MilliSatoshi
	idx  int
}

type dijkstraQueue []*dijkstraItem

func (q dijkstraQueue) Len() int           { return len(q) }
func (q dijkstraQueue) Less(i, j int) bool { return q[i].cost < q[j].cost }
func (q dijkstraQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i]; q[i].idx = i; q[j].idx = j }
func (q *dijkstraQueue) Push(x any) {
	item := x.(*dijkstraItem)
	item.idx = len(*q)
	*q = append(*q, item)
}
func (q *dijkstraQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[:n-1]
	return item
}

// findRoute computes the cheapest usable path delivering amt to the target,
// walking backward from the target so fees accumulate correctly.
func (r *candidateRouter) findRoute(amt lnwire.MilliSatoshi) (*route.Route,
	error) {

	// dist[node] = amount that must arrive at node to deliver amt.
	dist := make(map[route.Vertex]lnwire.MilliSatoshi)
	next := make(map[route.Vertex]*candidateEdge)

	dist[r.spec.Target] = amt
	pq := &dijkstraQueue{}
	heap.Push(pq, &dijkstraItem{node: r.spec.Target, cost: amt})

	for pq.Len() > 0 {
		item := heap.Pop(pq).(*dijkstraItem)
		node, arriving := item.node, item.cost

		if arriving > dist[node] {
			continue
		}
		if node == r.source {
			break
		}

		// Consider all edges INTO node: for edge u->node, u must
		// send arriving plus u's fee.
		for _, edge := range r.incomingEdges[node] {
			amtOver := arriving

			if !edge.usable(amtOver) {
				continue
			}

			// Skip channels whose liquidity failure bound says
			// this amount cannot pass.
			if bound, ok := r.failedAmt[edge.chanID]; ok &&
				amtOver >= bound {

				continue
			}

			// Our own channels: check exact local balance.
			if edge.from == r.source {
				if r.localBalances[edge.chanID] < amtOver {
					continue
				}
			}

			var sending lnwire.MilliSatoshi
			if edge.from == r.source {
				// We pay no fee to ourselves.
				sending = amtOver
			} else {
				sending = amtOver + edge.fee(amtOver)
			}

			best, ok := dist[edge.from]
			if !ok || sending < best {
				dist[edge.from] = sending
				next[edge.from] = edge
				heap.Push(pq, &dijkstraItem{
					node: edge.from,
					cost: sending,
				})
			}
		}
	}

	if _, ok := dist[r.source]; !ok {
		return nil, errors.New("no route found")
	}

	return r.buildRoute(amt, next)
}

// buildRoute walks the next-pointers from source to target and constructs a
// route with correctly accumulated fees and cltv deltas.
func (r *candidateRouter) buildRoute(amt lnwire.MilliSatoshi,
	next map[route.Vertex]*candidateEdge) (*route.Route, error) {

	const finalCltvDelta = 40

	// Collect the path edges source -> target.
	var path []*candidateEdge
	for node := r.source; node != r.spec.Target; {
		edge, ok := next[node]
		if !ok {
			return nil, fmt.Errorf("broken path at %v", node)
		}
		path = append(path, edge)
		node = edge.to
	}

	// Amounts and expiries per channel, computed backward.
	numHops := len(path)
	amtOver := make([]lnwire.MilliSatoshi, numHops)
	expiryOver := make([]uint32, numHops)

	amtOver[numHops-1] = amt
	expiryOver[numHops-1] = finalCltvDelta

	for i := numHops - 2; i >= 0; i-- {
		fwd := path[i+1]
		amtOver[i] = amtOver[i+1] + fwd.fee(amtOver[i+1])
		expiryOver[i] = expiryOver[i+1] +
			uint32(fwd.timeLockDelta)
	}

	hops := make([]*route.Hop, numHops)
	for i, edge := range path {
		amtToFwd := amt
		outgoingExpiry := uint32(finalCltvDelta)
		if i < numHops-1 {
			amtToFwd = amtOver[i+1]
			outgoingExpiry = expiryOver[i+1]
		}

		hops[i] = &route.Hop{
			PubKeyBytes:      edge.to,
			ChannelID:        edge.chanID,
			AmtToForward:     amtToFwd,
			OutgoingTimeLock: outgoingExpiry,
		}
	}

	return &route.Route{
		TotalTimeLock: expiryOver[0],
		TotalAmount:   amtOver[0],
		SourcePubKey:  r.source,
		Hops:          hops,
	}, nil
}

// RequestRoute returns the next route to try: the cheapest path for the
// current shard size, halving the shard when no route exists.
//
// NOTE: Part of the routing.SimRouter interface.
func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if r.shardAmt > amt {
		r.shardAmt = amt
	}

	for {
		rt, err := r.findRoute(r.shardAmt)
		if err == nil {
			return rt, nil
		}

		// No route at this shard size: split if we're allowed more
		// parts and the shard is still meaningfully large.
		partsLeft := r.spec.MaxParts - inFlightHtlcs
		if partsLeft <= 1 || r.shardAmt < 10_000_000 {
			return nil, err
		}
		r.shardAmt /= 2
	}
}

// ReportAttempt learns from an attempt: liquidity failures set an upper
// bound on the failing channel.
//
// NOTE: Part of the routing.SimRouter interface.
func (r *candidateRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if result.Failure == nil {
		return nil
	}

	// Locate the failing hop and record the amount bound on its
	// outgoing channel.
	failIdx := -1
	if result.FailureSource == rt.SourcePubKey {
		failIdx = 0
	}
	for i, hop := range rt.Hops {
		if hop.PubKeyBytes == result.FailureSource {
			failIdx = i + 1
		}
	}

	// The failing node could not forward over its outgoing channel,
	// which is rt.Hops[failIdx].
	if failIdx >= 0 && failIdx < len(rt.Hops) {
		hop := rt.Hops[failIdx]
		amtOver := rt.TotalAmount
		if failIdx > 0 {
			amtOver = rt.Hops[failIdx-1].AmtToForward
		}

		bound, ok := r.failedAmt[hop.ChannelID]
		if !ok || amtOver < bound {
			r.failedAmt[hop.ChannelID] = amtOver
		}
	}

	return nil
}
