package main

// Evolved candidate: bimodal-prior probabilistic routing with per-directed
// channel liquidity beliefs, probability-weighted path search, and joint
// (up-front) MPP route-set planning sized to believed corridor capacity.
//
// Key changes vs prior version:
//   - The single-path shortcut no longer hogs the whole payment when the
//     amount is large relative to believed corridor capacity: we only send
//     one full-amount route when the believed bottleneck can actually carry
//     it (or no parts are left). Big payments go straight to joint planning.
//   - Joint planning is genuinely min-cost-flow-ish: candidate corridors are
//     enumerated once per plan with per-edge reservations, and shard sizes
//     come from believed edge capacity rather than blind halving. Corridors
//     are excluded by their whole edge set (not just the bottleneck), so
//     shards do not silently contend.
//   - Route amounts respect minHTLC/maxHTLC of every hop when trimming.
//   - Attempt budget scales with MaxParts and no longer wastes calls: a
//     plan is only discarded when the underlying beliefs invalidate it, and
//     residual planning reuses already-known bounds.
//   - Failure bounds are refreshable: a hard upper-fail bound is allowed one
//     "retry-at-lower-amount" ladder, and repeated whole-plan failure relaxes
//     bounds slightly so a drifting network can be re-probed instead of the
//     graph becoming permanently unroutable (the main cause of the observed
//     "attempt budget exhausted" terminal failures).

import (
	"container/heap"
	"context"
	"errors"
	"math"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	finalCltvDelta = 40

	probFloor = 0.005
	probCap   = 0.985

	probKnownOK  = 0.995
	probKnownBad = 0.0005

	// attempt cost model: an attempt is worth this many msat of fee.
	attemptCostMsat = 15_000.0

	// minimum meaningful shard.
	minShard = lnwire.MilliSatoshi(500_000)

	maxHops = 6

	// hard ceiling on RequestRoute calls per payment.
	maxAttemptsBase = 48
)

// ---------------------------------------------------------------------------
// Graph
// ---------------------------------------------------------------------------

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

func (e *candidateEdge) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return e.baseFeeMsat + amt*e.feeRatePPM/1_000_000
}

func (e *candidateEdge) policyOK(amt lnwire.MilliSatoshi) bool {
	if amt < e.minHTLC {
		return false
	}
	if e.maxHTLC != 0 && amt > e.maxHTLC {
		return false
	}
	return amt <= e.capacity
}

// belief is what we know about a directed channel's liquidity.
type belief struct {
	// lowerOK is the largest amount proven to pass.
	lowerOK lnwire.MilliSatoshi
	// upperFail is the smallest amount proven to fail (0 = unknown).
	upperFail lnwire.MilliSatoshi
	hasFail   bool
	// fails counts how many times this edge has failed this session.
	fails int
	// inFlight is liquidity our own unsettled shards hold on this edge.
	inFlight lnwire.MilliSatoshi
}

type dirKey struct {
	chanID uint64
	to     route.Vertex
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edgeIndex     map[dirKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	beliefs map[dirKey]*belief

	// plan is the current up-front route-set plan (queued shards).
	plan []*route.Route

	attempts int

	// consecutive failures with no progress at all.
	dryRounds int

	lastRemaining lnwire.MilliSatoshi
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	r := &candidateRouter{
		source:        source,
		spec:          spec,
		incomingEdges: make(map[route.Vertex][]*candidateEdge),
		edgeIndex:     make(map[dirKey]*candidateEdge),
		localBalances: localBalances,
		beliefs:       make(map[dirKey]*belief),
		lastRemaining: spec.Amount,
	}

	ctx := context.Background()
	seen := map[route.Vertex]bool{source: true}
	queue := []route.Vertex{source}

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

				e := &candidateEdge{
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
					e.maxHTLC = pol.MaxHTLC
				}

				k := dirKey{chanID: e.chanID, to: e.to}
				if _, dup := r.edgeIndex[k]; dup {
					return nil
				}
				r.edgeIndex[k] = e
				r.incomingEdges[e.to] = append(
					r.incomingEdges[e.to], e,
				)

				return nil
			}, func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	return r, nil
}

// ---------------------------------------------------------------------------
// Probability model
// ---------------------------------------------------------------------------

func (r *candidateRouter) key(e *candidateEdge) dirKey {
	return dirKey{chanID: e.chanID, to: e.to}
}

func (r *candidateRouter) beliefOf(k dirKey) *belief {
	b, ok := r.beliefs[k]
	if !ok {
		b = &belief{}
		r.beliefs[k] = b
	}
	return b
}

// bimodalPrior gives P(channel can forward amt) with no evidence, assuming
// funds sit almost entirely on one side of the channel.
func bimodalPrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity == 0 {
		return probFloor
	}
	if amt > capacity {
		return 0
	}
	x := float64(amt) / float64(capacity)

	low := math.Exp(-x / 0.035)
	high := 0.52 / (1.0 + math.Exp((x-0.72)/0.11))

	p := 0.42*low + high
	if p > probCap {
		p = probCap
	}
	if p < probFloor {
		p = probFloor
	}
	return p
}

// availLocal is our exact remaining outbound balance on a local channel.
func (r *candidateRouter) availLocal(e *candidateEdge) lnwire.MilliSatoshi {
	bal := r.localBalances[e.chanID]
	if b, ok := r.beliefs[r.key(e)]; ok {
		if b.inFlight >= bal {
			return 0
		}
		bal -= b.inFlight
	}
	return bal
}

// successProb blends hard evidence with the prior for sending amt over e,
// accounting for liquidity our own in-flight shards already hold.
func (r *candidateRouter) successProb(e *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if e.from == r.source {
		if amt <= r.availLocal(e) {
			return probKnownOK
		}
		return 0
	}

	b := r.beliefs[r.key(e)]
	eff := amt
	if b != nil {
		eff = amt + b.inFlight
	}
	if eff > e.capacity {
		return probKnownBad
	}

	if b == nil {
		return bimodalPrior(amt, e.capacity)
	}

	if eff <= b.lowerOK {
		return probKnownOK
	}
	if b.hasFail && eff >= b.upperFail {
		// Not a permanent veto: repeated whole-payment stalls and a
		// moving network mean an old bound may be stale. Give a tiny
		// but non-zero chance that grows with dry rounds, so search
		// can re-probe rather than declaring the graph unroutable.
		if r.dryRounds >= 2 && eff < e.capacity {
			return probFloor * float64(r.dryRounds)
		}
		return probKnownBad
	}

	lo := b.lowerOK
	hi := e.capacity
	if b.hasFail && b.upperFail < hi {
		hi = b.upperFail
	}
	if hi <= lo {
		return probFloor
	}
	frac := float64(eff-lo) / float64(hi-lo)
	base := bimodalPrior(eff, e.capacity)

	p := base
	if b.hasFail {
		p = base * (1.0 - 0.8*frac)
	}
	if b.lowerOK > 0 {
		p = p*0.55 + 0.45*(1.0-frac)*probCap
	}
	if p > probCap {
		p = probCap
	}
	if p < probFloor {
		p = probFloor
	}
	return p
}

// ---------------------------------------------------------------------------
// Believed capacity
// ---------------------------------------------------------------------------

// edgeCapacityGuess estimates how much this directed edge can plausibly
// carry right now, net of our own in-flight reservations.
func (r *candidateRouter) edgeCapacityGuess(e *candidateEdge) lnwire.MilliSatoshi {
	if e.from == r.source {
		c := r.availLocal(e)
		if e.maxHTLC != 0 && e.maxHTLC < c {
			c = e.maxHTLC
		}
		return c
	}

	// Bimodal channels usually hold most funds on one side, so a large
	// fraction is plausibly available unless evidence says otherwise.
	c := e.capacity * 7 / 10

	b := r.beliefs[r.key(e)]
	if b != nil {
		if b.hasFail && b.upperFail > 0 {
			// Stay meaningfully below the proven failure point:
			// the retry-at-lower-amount factor.
			lim := b.upperFail / 2
			if lim < c {
				c = lim
			}
		}
		if b.lowerOK > c {
			c = b.lowerOK
		}
		if b.inFlight >= c {
			c = 0
		} else {
			c -= b.inFlight
		}
	}

	if e.maxHTLC != 0 && e.maxHTLC < c {
		c = e.maxHTLC
	}
	if e.capacity < c {
		c = e.capacity
	}
	return c
}

// ---------------------------------------------------------------------------
// Probability-weighted search
// ---------------------------------------------------------------------------

type pqItem struct {
	node route.Vertex
	cost float64
	amt  lnwire.MilliSatoshi
	prob float64
	hops int
	idx  int
}

type pQueue []*pqItem

func (q pQueue) Len() int           { return len(q) }
func (q pQueue) Less(i, j int) bool { return q[i].cost < q[j].cost }
func (q pQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].idx = i
	q[j].idx = j
}
func (q *pQueue) Push(x any) { it := x.(*pqItem); it.idx = len(*q); *q = append(*q, it) }
func (q *pQueue) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}

// findRoute searches backward from the target for the path minimising
// fee + attemptCost/probability, skipping any directed channel in `avoid`.
// It returns the built route, its end-to-end success probability, and the
// believed bottleneck capacity of the corridor.
func (r *candidateRouter) findRoute(amt lnwire.MilliSatoshi,
	avoid map[dirKey]bool) (*route.Route, float64, lnwire.MilliSatoshi,
	error) {

	type state struct {
		cost float64
		amt  lnwire.MilliSatoshi
		prob float64
		hops int
		edge *candidateEdge
	}

	best := make(map[route.Vertex]*state, 64)
	best[r.spec.Target] = &state{amt: amt, prob: 1.0}

	pq := &pQueue{}
	heap.Push(pq, &pqItem{node: r.spec.Target, amt: amt, prob: 1.0})

	for pq.Len() > 0 {
		it := heap.Pop(pq).(*pqItem)
		cur := best[it.node]
		if cur == nil || it.cost > cur.cost+1e-9 {
			continue
		}
		if it.node == r.source {
			break
		}
		if it.hops >= maxHops {
			continue
		}

		for _, e := range r.incomingEdges[it.node] {
			if avoid[r.key(e)] {
				continue
			}
			amtOver := it.amt
			if !e.policyOK(amtOver) {
				continue
			}

			p := r.successProb(e, amtOver)
			if p <= probKnownBad {
				continue
			}

			var sending lnwire.MilliSatoshi
			if e.from == r.source {
				sending = amtOver
			} else {
				sending = amtOver + e.fee(amtOver)
			}

			newProb := it.prob * p
			if newProb < 5e-5 {
				continue
			}

			feePart := float64(sending - amt)
			riskPart := attemptCostMsat / newProb
			hopPart := float64(it.hops+1) * 250.0

			cost := feePart + riskPart + hopPart

			prev := best[e.from]
			if prev != nil && prev.cost <= cost {
				continue
			}
			best[e.from] = &state{
				cost: cost, amt: sending, prob: newProb,
				hops: it.hops + 1, edge: e,
			}
			heap.Push(pq, &pqItem{
				node: e.from, cost: cost, amt: sending,
				prob: newProb, hops: it.hops + 1,
			})
		}
	}

	src := best[r.source]
	if src == nil {
		return nil, 0, 0, errors.New("no route found")
	}

	var path []*candidateEdge
	node := r.source
	for node != r.spec.Target {
		st := best[node]
		if st == nil || st.edge == nil {
			return nil, 0, 0, errors.New("broken path")
		}
		path = append(path, st.edge)
		node = st.edge.to
		if len(path) > maxHops+1 {
			return nil, 0, 0, errors.New("path too long")
		}
	}

	rt, err := buildRoute(r.source, amt, path)
	if err != nil {
		return nil, 0, 0, err
	}

	// Believed bottleneck of the corridor, measured at the amount that
	// actually flows over each hop.
	bottleneck := lnwire.MilliSatoshi(math.MaxUint64 >> 1)
	for i, e := range path {
		c := r.edgeCapacityGuess(e)
		// Deduct the fee overhead carried by upstream hops so the
		// bottleneck is expressed in delivered terms.
		over := routeAmt(rt, i)
		deliver := deliveredAmt(rt)
		if over > deliver && c > over-deliver {
			c -= over - deliver
		} else if over > deliver {
			c = 0
		}
		if c < bottleneck {
			bottleneck = c
		}
	}

	return rt, src.prob, bottleneck, nil
}

func buildRoute(source route.Vertex, amt lnwire.MilliSatoshi,
	path []*candidateEdge) (*route.Route, error) {

	n := len(path)
	if n == 0 {
		return nil, errors.New("empty path")
	}

	amtOver := make([]lnwire.MilliSatoshi, n)
	expiryOver := make([]uint32, n)
	amtOver[n-1] = amt
	expiryOver[n-1] = finalCltvDelta

	for i := n - 2; i >= 0; i-- {
		fwd := path[i+1]
		amtOver[i] = amtOver[i+1] + fwd.fee(amtOver[i+1])
		expiryOver[i] = expiryOver[i+1] + uint32(fwd.timeLockDelta)
	}

	// Every hop must satisfy its own policy at the amount it carries.
	for i, e := range path {
		if !e.policyOK(amtOver[i]) {
			return nil, errors.New("policy violation")
		}
	}

	hops := make([]*route.Hop, n)
	for i, e := range path {
		amtToFwd := amt
		outExpiry := uint32(finalCltvDelta)
		if i < n-1 {
			amtToFwd = amtOver[i+1]
			outExpiry = expiryOver[i+1]
		}
		hops[i] = &route.Hop{
			PubKeyBytes:      e.to,
			ChannelID:        e.chanID,
			AmtToForward:     amtToFwd,
			OutgoingTimeLock: outExpiry,
		}
	}

	return &route.Route{
		TotalTimeLock: expiryOver[0],
		TotalAmount:   amtOver[0],
		SourcePubKey:  source,
		Hops:          hops,
	}, nil
}

// ---------------------------------------------------------------------------
// Belief bookkeeping over routes
// ---------------------------------------------------------------------------

// routeAmt returns the amount flowing over hop i of rt.
func routeAmt(rt *route.Route, i int) lnwire.MilliSatoshi {
	if i == 0 {
		return rt.TotalAmount
	}
	return rt.Hops[i-1].AmtToForward
}

func deliveredAmt(rt *route.Route) lnwire.MilliSatoshi {
	if len(rt.Hops) == 0 {
		return rt.TotalAmount
	}
	return rt.Hops[len(rt.Hops)-1].AmtToForward
}

func (r *candidateRouter) dirKeyForHop(rt *route.Route, i int) dirKey {
	return dirKey{chanID: rt.Hops[i].ChannelID, to: rt.Hops[i].PubKeyBytes}
}

func (r *candidateRouter) edgeOf(rt *route.Route, i int) *candidateEdge {
	return r.edgeIndex[r.dirKeyForHop(rt, i)]
}

// reserve/release track our own in-flight usage of each corridor so that
// sibling shards do not lean on the same liquidity twice.
func (r *candidateRouter) reserve(rt *route.Route) {
	for i := range rt.Hops {
		b := r.beliefOf(r.dirKeyForHop(rt, i))
		b.inFlight += routeAmt(rt, i)
	}
}

func (r *candidateRouter) release(rt *route.Route) {
	for i := range rt.Hops {
		if b, ok := r.beliefs[r.dirKeyForHop(rt, i)]; ok {
			a := routeAmt(rt, i)
			if b.inFlight >= a {
				b.inFlight -= a
			} else {
				b.inFlight = 0
			}
		}
	}
}

func (r *candidateRouter) markOK(rt *route.Route, upto int) {
	for i := 0; i < upto && i < len(rt.Hops); i++ {
		b := r.beliefOf(r.dirKeyForHop(rt, i))
		a := routeAmt(rt, i)
		if a > b.lowerOK {
			b.lowerOK = a
		}
		// Success at or above a recorded failure bound proves the old
		// bound is stale (drift): clear it.
		if b.hasFail && b.upperFail <= a {
			b.hasFail = false
			b.upperFail = 0
			b.fails = 0
		}
	}
}

func (r *candidateRouter) markFail(rt *route.Route, idx int) {
	if idx < 0 || idx >= len(rt.Hops) {
		return
	}
	b := r.beliefOf(r.dirKeyForHop(rt, idx))
	a := routeAmt(rt, idx)
	if !b.hasFail || a < b.upperFail {
		b.hasFail = true
		b.upperFail = a
	}
	b.fails++
	if b.lowerOK >= a {
		if a > 1 {
			b.lowerOK = a - 1
		} else {
			b.lowerOK = 0
		}
	}
}

// ---------------------------------------------------------------------------
// Joint route-set planning
// ---------------------------------------------------------------------------

// trimTo returns a route delivering at most want over the same path, or nil
// if no valid amount in [minShard, want] satisfies every hop policy.
func (r *candidateRouter) trimTo(rt *route.Route,
	want lnwire.MilliSatoshi) *route.Route {

	if want < minShard {
		return nil
	}
	path := make([]*candidateEdge, len(rt.Hops))
	for i := range rt.Hops {
		e := r.edgeOf(rt, i)
		if e == nil {
			return nil
		}
		path[i] = e
	}
	out, err := buildRoute(r.source, want, path)
	if err != nil {
		return nil
	}
	return out
}

// planRouteSet builds up to maxParts routes jointly, sizing each shard to
// the corridor it rides so that parallel corridors of unequal capacity each
// carry what they can bear. Reservations are applied while planning and then
// rolled back; the caller reserves shards as it dispatches them.
func (r *candidateRouter) planRouteSet(total lnwire.MilliSatoshi,
	maxParts int) []*route.Route {

	if maxParts < 1 {
		maxParts = 1
	}

	var out []*route.Route
	var reserved []*route.Route
	defer func() {
		for _, rt := range reserved {
			r.release(rt)
		}
	}()

	remaining := total
	avoid := make(map[dirKey]bool)

	for part := 0; part < maxParts && remaining >= minShard; part++ {
		rt, _, bottleneck, err := r.findRoute(remaining, avoid)
		if err != nil {
			// The full residue does not route; look for a corridor
			// that can carry a smaller slice.
			trial := remaining / 2
			for trial >= minShard {
				rt, _, bottleneck, err = r.findRoute(
					trial, avoid,
				)
				if err == nil {
					break
				}
				trial /= 2
			}
			if err != nil {
				break
			}
		}

		want := deliveredAmt(rt)
		if bottleneck < want {
			want = bottleneck
		}
		if want > remaining {
			want = remaining
		}

		if want < deliveredAmt(rt) {
			trimmed := r.trimTo(rt, want)
			if trimmed == nil {
				// Cannot legally shrink onto this corridor:
				// exclude its bottleneck and move on.
				if k, ok := r.weakestKey(rt); ok {
					avoid[k] = true
					continue
				}
				break
			}
			rt = trimmed
		}

		if deliveredAmt(rt) < minShard {
			if k, ok := r.weakestKey(rt); ok {
				avoid[k] = true
				continue
			}
			break
		}

		out = append(out, rt)
		r.reserve(rt)
		reserved = append(reserved, rt)

		d := deliveredAmt(rt)
		if d >= remaining {
			remaining = 0
		} else {
			remaining -= d
		}

		// Exclude every non-local hop of the placed shard so the next
		// shard genuinely explores a different corridor instead of
		// stacking onto liquidity we already committed.
		for i := range rt.Hops {
			e := r.edgeOf(rt, i)
			if e == nil {
				continue
			}
			if e.from == r.source && r.availLocal(e) >= minShard {
				// Local channel with headroom left: reusable.
				continue
			}
			avoid[r.key(e)] = true
		}
	}

	return out
}

// weakestKey returns the directed channel with the least believed headroom.
func (r *candidateRouter) weakestKey(rt *route.Route) (dirKey, bool) {
	var bk dirKey
	var min lnwire.MilliSatoshi
	found := false
	for i := range rt.Hops {
		e := r.edgeOf(rt, i)
		if e == nil {
			continue
		}
		c := r.edgeCapacityGuess(e)
		if !found || c < min {
			min = c
			bk = r.key(e)
			found = true
		}
	}
	return bk, found
}

// ---------------------------------------------------------------------------
// SimRouter interface
// ---------------------------------------------------------------------------

// RequestRoute returns the next route to try. It plans a whole route set up
// front and hands the shards out one per call so MaxParts fill quickly and
// commit before the network drifts.
//
// NOTE: Part of the routing.SimRouter interface.
func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	budget := maxAttemptsBase
	if r.spec.MaxParts > 1 {
		budget += int(r.spec.MaxParts) * 4
	}
	r.attempts++
	if r.attempts > budget {
		return nil, errors.New("attempt budget exhausted")
	}

	if amt != r.lastRemaining {
		// Progress: a shard settled, so the queued plan is stale.
		r.plan = nil
		r.lastRemaining = amt
		r.dryRounds = 0
	}

	// Hand out queued shards that still fit and still look plausible.
	for len(r.plan) > 0 {
		rt := r.plan[0]
		r.plan = r.plan[1:]
		if deliveredAmt(rt) <= amt && r.routeStillPlausible(rt) {
			r.dispatch(rt)
			return rt, nil
		}
	}

	partsLeft := 1
	if uint32(r.spec.MaxParts) > inFlightHtlcs {
		partsLeft = int(uint32(r.spec.MaxParts) - inFlightHtlcs)
	}

	// Single-route attempt for the whole residue: cheapest and fewest
	// attempts, but only when the corridor is believed able to bear it
	// (otherwise we are just buying a guaranteed failure).
	if rt, prob, bottleneck, err := r.findRoute(amt, nil); err == nil {
		fits := bottleneck >= deliveredAmt(rt)
		if partsLeft <= 1 || (fits && prob >= 0.25) {
			r.dispatch(rt)
			return rt, nil
		}
	}

	plan := r.planRouteSet(amt, partsLeft)

	// If the plan covers nothing useful, fall back to any single route we
	// can send, shrinking until something is routable.
	if len(plan) == 0 {
		trial := amt
		for i := 0; i < 8 && trial >= minShard; i++ {
			if rt, _, _, err := r.findRoute(trial, nil); err == nil {
				r.dispatch(rt)
				return rt, nil
			}
			trial /= 2
		}
		return nil, errors.New("no route found")
	}

	r.plan = plan[1:]
	rt := plan[0]
	r.dispatch(rt)
	return rt, nil
}

func (r *candidateRouter) dispatch(rt *route.Route) {
	r.reserve(rt)
}

// routeStillPlausible re-checks a queued shard against current beliefs.
func (r *candidateRouter) routeStillPlausible(rt *route.Route) bool {
	for i := range rt.Hops {
		e := r.edgeOf(rt, i)
		if e == nil {
			return false
		}
		if r.successProb(e, routeAmt(rt, i)) <= probKnownBad {
			return false
		}
	}
	return true
}

// ReportAttempt folds attempt feedback into the liquidity beliefs.
//
// NOTE: Part of the routing.SimRouter interface.
func (r *candidateRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if rt == nil {
		return nil
	}

	if result.Failure == nil {
		// Settled (or held, under atomic MPP): the whole path passed.
		r.markOK(rt, len(rt.Hops))
		r.dryRounds = 0
		// Keep the reservation: under atomic MPP the shard still holds
		// liquidity; under plain MPP the balance genuinely moved.
		if len(rt.Hops) > 0 {
			e := r.edgeOf(rt, 0)
			if e != nil && e.from == r.source {
				a := routeAmt(rt, 0)
				if r.localBalances[e.chanID] >= a {
					r.localBalances[e.chanID] -= a
				} else {
					r.localBalances[e.chanID] = 0
				}
				// The reservation is now folded into the
				// reduced balance; drop it to avoid double
				// counting on the local hop.
				if b, ok := r.beliefs[r.key(e)]; ok {
					if b.inFlight >= a {
						b.inFlight -= a
					} else {
						b.inFlight = 0
					}
				}
			}
		}
		return nil
	}

	// Failure: this shard holds nothing any more.
	r.release(rt)
	r.dryRounds++

	// The remaining queued plan was sized assuming this corridor worked;
	// re-plan with the new evidence rather than firing known-stale shards.
	r.plan = nil

	failIdx := -1
	if result.FailureSource == rt.SourcePubKey {
		failIdx = 0
	} else {
		for i, hop := range rt.Hops {
			if hop.PubKeyBytes == result.FailureSource {
				failIdx = i + 1
				break
			}
		}
	}

	if failIdx < 0 {
		// Unknown failure source: do not poison specific channels,
		// but mildly distrust the weakest hop so we vary the route.
		if k, ok := r.weakestKey(rt); ok {
			b := r.beliefOf(k)
			b.fails++
		}
		return nil
	}

	// Everything before the failing hop demonstrably forwarded.
	r.markOK(rt, failIdx)

	if failIdx < len(rt.Hops) {
		r.markFail(rt, failIdx)
	}

	return nil
}
