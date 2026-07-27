package main

// Candidate: log_bimodal_cost
// Mechanism: Replace linear capacity utilization penalty (capped at 100 ppm)
// with a log-probability penalty based on the bimodal prior.
//
// Current capacity_penalty: penalty = 0 for util <= 70%, linearly grows 0..100 ppm
// for util 70%..100%. The hard cap means a channel at 95% is penalized identically
// to one at 100% — the penalty fails to distinguish near-cliff from past-cliff.
//
// This candidate: penalty = -log(P_bimodal(util)) * scale_ppm
//   P_bimodal = 1 / (1 + exp(10 * (util - 0.7)))  [logistic cliff at 70%]
//   scale_ppm = 40  [penalty in ppm-equivalent units]
//
// Key properties:
//   - At util=0.70: P=0.50, -log(0.50)*40 = 27.7 ppm  (starts penalizing right at 70%)
//   - At util=0.80: P=0.12, -log(0.12)*40 = 85 ppm    (steeper than linear 33 ppm)
//   - At util=0.90: P=0.02, -log(0.02)*40 = 153 ppm   (much steeper than linear 67 ppm)
//   - At util=0.99: P=0.003, -log(0.003)*40 = 234 ppm (NO CAP — forces high-capacity paths)
//   - At util=0.50: P=0.98, -log(0.98)*40 ≈ 0.8 ppm  (near-zero for safe channels)
//
// The UNBOUNDED growth near 100% utilization is the key difference: it forces the
// router to seek higher-capacity alternatives proportionally to the cliff steepness,
// not just up to a 100 ppm ceiling that quickly becomes irrelevant vs fees.

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

type failKey struct {
	chanID uint64
	from   route.Vertex
}

var globalFailedAmt = map[failKey]lnwire.MilliSatoshi{}

type candidateEdge struct {
	chanID        uint64
	from, to      route.Vertex
	capacity      lnwire.MilliSatoshi
	baseFeeMsat   lnwire.MilliSatoshi
	feeRatePPM    lnwire.MilliSatoshi
	timeLockDelta uint16
	minHTLC       lnwire.MilliSatoshi
	maxHTLC       lnwire.MilliSatoshi
}

func (e *candidateEdge) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return e.baseFeeMsat + amt*e.feeRatePPM/1_000_000
}

func (e *candidateEdge) usable(amt lnwire.MilliSatoshi) bool {
	if amt < e.minHTLC {
		return false
	}
	if e.maxHTLC != 0 && amt > e.maxHTLC {
		return false
	}
	return amt <= e.capacity
}

type shardPlan struct {
	route  *route.Route
	amount lnwire.MilliSatoshi
}

type candidateRouter struct {
	source        route.Vertex
	spec          *routing.SimPaymentSpec
	incomingEdges map[route.Vertex][]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi
	failedAmt     map[failKey]lnwire.MilliSatoshi
	plan          []shardPlan
	planIdx       int
	shardAmt      lnwire.MilliSatoshi
	pending       map[uint64]*route.Route
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	failedAmt := make(map[failKey]lnwire.MilliSatoshi, len(globalFailedAmt))
	for k, v := range globalFailedAmt {
		failedAmt[k] = v
	}

	router := &candidateRouter{
		source:        source,
		spec:          spec,
		incomingEdges: make(map[route.Vertex][]*candidateEdge),
		localBalances: localBalances,
		failedAmt:     failedAmt,
		shardAmt:      spec.Amount,
		pending:       make(map[uint64]*route.Route),
	}

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
					chanID:        ch.ChannelID,
					from:          ch.OtherNode,
					to:            node,
					capacity:      lnwire.NewMSatFromSatoshis(ch.Capacity),
					baseFeeMsat:   pol.FeeBaseMSat,
					feeRatePPM:    pol.FeeProportionalMillionths,
					timeLockDelta: pol.TimeLockDelta,
					minHTLC:       pol.MinHTLC,
				}
				if pol.HasMaxHTLC {
					edge.maxHTLC = pol.MaxHTLC
				}
				router.incomingEdges[edge.to] = append(router.incomingEdges[edge.to], edge)
				return nil
			}, func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	return router, nil
}

type dijkstraItem struct {
	node route.Vertex
	cost lnwire.MilliSatoshi
	idx  int
}

type dijkstraQueue []*dijkstraItem

func (q dijkstraQueue) Len() int           { return len(q) }
func (q dijkstraQueue) Less(i, j int) bool { return q[i].cost < q[j].cost }
func (q dijkstraQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].idx = i
	q[j].idx = j
}
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

func (r *candidateRouter) findRoute(amt lnwire.MilliSatoshi,
	exclude map[uint64]struct{}) (*route.Route, error) {

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

		for _, edge := range r.incomingEdges[node] {
			amtOver := arriving
			if !edge.usable(amtOver) {
				continue
			}
			if _, ex := exclude[edge.chanID]; ex {
				continue
			}
			if bound, ok := r.failedAmt[failKey{edge.chanID, edge.from}]; ok && amtOver >= bound {
				continue
			}
			if edge.from == r.source && r.localBalances[edge.chanID] < amtOver {
				continue
			}

			var sending lnwire.MilliSatoshi
			if edge.from == r.source {
				sending = amtOver
			} else {
				sending = amtOver + edge.fee(amtOver)
			}

			// Log-probability bimodal penalty: -log(P_bimodal(util)) * 40 ppm
			// P_bimodal uses a logistic cliff at 70% utilization, steepness 10.
			// Unlike the linear cap (0..100 ppm), this grows without bound near 100%
			// utilization, proportionally penalizing channels near the cliff.
			if edge.capacity > 0 {
				util := float64(amtOver) / float64(edge.capacity)
				if util > 0.01 && util < 1.0 {
					p := 1.0 / (1.0 + math.Exp(10.0*(util-0.7)))
					if p < 0.001 {
						p = 0.001
					}
					logPenaltyPPM := lnwire.MilliSatoshi(-math.Log(p) * 40)
					sending += amtOver * logPenaltyPPM / 1_000_000
				}
			}

			if best, ok := dist[edge.from]; !ok || sending < best {
				dist[edge.from] = sending
				next[edge.from] = edge
				heap.Push(pq, &dijkstraItem{node: edge.from, cost: sending})
			}
		}
	}

	if _, ok := dist[r.source]; !ok {
		return nil, errors.New("no route found")
	}
	return r.buildRoute(amt, next)
}

func (r *candidateRouter) buildRoute(amt lnwire.MilliSatoshi,
	next map[route.Vertex]*candidateEdge) (*route.Route, error) {

	const finalCltvDelta = 40

	var path []*candidateEdge
	for node := r.source; node != r.spec.Target; {
		edge, ok := next[node]
		if !ok {
			return nil, fmt.Errorf("broken path at %v", node)
		}
		path = append(path, edge)
		node = edge.to
	}

	numHops := len(path)
	amtOver := make([]lnwire.MilliSatoshi, numHops)
	expiryOver := make([]uint32, numHops)

	amtOver[numHops-1] = amt
	expiryOver[numHops-1] = finalCltvDelta

	for i := numHops - 2; i >= 0; i-- {
		fwd := path[i+1]
		amtOver[i] = amtOver[i+1] + fwd.fee(amtOver[i+1])
		expiryOver[i] = expiryOver[i+1] + uint32(fwd.timeLockDelta)
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

func (r *candidateRouter) buildPlan(totalAmt lnwire.MilliSatoshi) []shardPlan {
	maxParts := int(r.spec.MaxParts)
	exclude := make(map[uint64]struct{})
	var plan []shardPlan
	remaining := totalAmt

	for i := 0; i < maxParts && remaining >= 1_000; i++ {
		slotsLeft := lnwire.MilliSatoshi(maxParts - i)
		shardSize := remaining / slotsLeft
		if shardSize < 1_000 {
			shardSize = 1_000
		}

		rt, err := r.findRoute(shardSize, exclude)
		if err != nil {
			break
		}

		plan = append(plan, shardPlan{route: rt, amount: shardSize})
		remaining -= shardSize
		for _, hop := range rt.Hops {
			exclude[hop.ChannelID] = struct{}{}
		}
	}

	return plan
}

func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if r.shardAmt > amt {
		r.shardAmt = amt
	}

	if r.planIdx < len(r.plan) {
		shard := r.plan[r.planIdx]
		r.planIdx++
		if shard.amount <= amt {
			return shard.route, nil
		}
		noEx := make(map[uint64]struct{})
		if rt, err := r.findRoute(amt, noEx); err == nil {
			return rt, nil
		}
		r.shardAmt = amt
	}

	if inFlightHtlcs == 0 && r.spec.MaxParts > 1 && amt >= 10_000_000 {
		r.plan = r.buildPlan(amt)
		r.planIdx = 0
		if len(r.plan) > 0 {
			shard := r.plan[0]
			r.planIdx = 1
			return shard.route, nil
		}
	}

	noExclude := make(map[uint64]struct{})
	for {
		rt, err := r.findRoute(r.shardAmt, noExclude)
		if err == nil {
			return rt, nil
		}
		partsLeft := r.spec.MaxParts - inFlightHtlcs
		if partsLeft <= 1 || r.shardAmt < 10_000_000 {
			return nil, err
		}
		r.shardAmt /= 2
	}
}

func (r *candidateRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if result.Failure == nil {
		return nil
	}

	r.plan = nil
	r.planIdx = 0

	failIdx := -1
	if result.FailureSource == rt.SourcePubKey {
		failIdx = 0
	}
	for i, hop := range rt.Hops {
		if hop.PubKeyBytes == result.FailureSource {
			failIdx = i + 1
		}
	}

	if failIdx >= 0 && failIdx < len(rt.Hops) {
		hop := rt.Hops[failIdx]
		amtOver := rt.TotalAmount
		if failIdx > 0 {
			amtOver = rt.Hops[failIdx-1].AmtToForward
		}

		var failFrom route.Vertex
		if failIdx == 0 {
			failFrom = rt.SourcePubKey
		} else {
			failFrom = rt.Hops[failIdx-1].PubKeyBytes
		}
		key := failKey{hop.ChannelID, failFrom}

		if bound, ok := r.failedAmt[key]; !ok || amtOver < bound {
			r.failedAmt[key] = amtOver
		}
		if bound, ok := globalFailedAmt[key]; !ok || amtOver < bound {
			globalFailedAmt[key] = amtOver
		}
	}

	return nil
}
