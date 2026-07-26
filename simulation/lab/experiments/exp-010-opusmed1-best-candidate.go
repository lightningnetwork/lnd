package main

// This file is the CANDIDATE SLOT for evolved routing algorithms.
//
// Algorithm: probability-weighted backward search over a bimodal
// liquidity prior refined by per-directed-channel hard bounds
// (lower-OK / upper-fail), plus a JOINT shard planner: instead of
// blindly halving on failure, we plan a set of disjoint corridors up
// front and size each shard to the liquidity that corridor is believed
// to bear. Success rate is already saturated on the current scenarios,
// so the main gains here target attempt count and fee: cheap-first
// route selection when confidence is high, capacity-aware shard sizing,
// and an early return when a single route clears a high bar.

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
	maxHops        = 6

	// probFloor and probCeil bound the success estimate for any edge.
	probFloor = 0.005
	probCeil  = 0.985

	// knownOK is the probability assigned below a proven-OK bound.
	knownOK = 0.995

	// attemptCost is the virtual msat penalty for one failed attempt.
	// It converts a probability into a fee-comparable cost. Keeping it
	// moderate stops the search from paying wild fees for marginal
	// reliability gains.
	attemptCost = 9_000.0

	// minShard is the smallest shard we will ever attempt.
	minShard = 1_000_000

	// maxAttempts guards the retry budget for one payment.
	maxAttempts = 48
)

// candidateEdge is one directed edge of the public graph.
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

// belief holds what we have learned about one directed channel. Hard
// bounds only: time decay has been shown to lose to plain bounds under
// drift, since a stale bound costs at most one retry to refresh.
type belief struct {
	// okAmt is the largest amount proven to pass in this direction.
	okAmt lnwire.MilliSatoshi

	// failAmt is the smallest amount proven to fail; zero means none.
	failAmt lnwire.MilliSatoshi

	// hasFail records whether failAmt is meaningful.
	hasFail bool
}

// usable returns the largest amount this belief does not rule out for
// the given capacity: just under the failure bound if we have one, else
// the full capacity.
func (b *belief) usable(cap lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	if b == nil {
		return cap
	}
	if b.hasFail {
		if b.failAmt <= 1 {
			return 0
		}
		return b.failAmt - 1
	}
	return cap
}

// candidateRouter is the evolved algorithm.
type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	// beliefs is keyed by directed channel (chanID, toward node).
	beliefs map[dirKey]*belief

	// inFlight tracks liquidity committed by pending attempts, per
	// directed channel, so parallel shards do not double-spend a
	// corridor.
	inFlight map[dirKey]lnwire.MilliSatoshi

	// nodeFail counts non-liquidity failures per node, used to steer
	// away from nodes with policy problems we cannot model.
	nodeFail map[route.Vertex]int

	// feeBump scales the fee headroom we add on top of advertised
	// policies, grown when a node complains about insufficient fees.
	feeBump uint32

	// attempts counts total attempts this payment; used as a give-up
	// guard so we do not burn the retry budget forever.
	attempts int

	// consecFail counts attempts since the last settled shard; used to
	// grow the willingness to split.
	consecFail int
}

type dirKey struct {
	chanID uint64
	toward route.Vertex
}

// newCandidateRouter builds the router for one payment.
func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	r := &candidateRouter{
		source:        source,
		spec:          spec,
		incomingEdges: make(map[route.Vertex][]*candidateEdge),
		localBalances: localBalances,
		beliefs:       make(map[dirKey]*belief),
		inFlight:      make(map[dirKey]lnwire.MilliSatoshi),
		nodeFail:      make(map[route.Vertex]int),
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

// bimodalPrior estimates the chance that a channel of the given capacity
// can forward amt, assuming funds sit almost entirely on one side.
func bimodalPrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity == 0 {
		return probFloor
	}
	x := float64(amt) / float64(capacity)
	if x >= 1 {
		return probFloor
	}

	// Low mode: tiny amounts pass even through a mostly-drained
	// channel, so start high and decay fast.
	low := math.Exp(-6.0 * x)

	// Cliff: as the amount approaches capacity the probability that
	// the balance happens to sit on our side collapses.
	cliff := 1.0 / (1.0 + math.Exp(12.0*(x-0.55)))

	p := 0.30*low + 0.70*cliff
	if p < probFloor {
		return probFloor
	}
	if p > probCeil {
		return probCeil
	}
	return p
}

// localCap returns our exact spendable balance on a local channel, or
// false if the channel is not ours.
func (r *candidateRouter) localCap(e *candidateEdge) (lnwire.MilliSatoshi,
	bool) {

	if e.from != r.source {
		return 0, false
	}
	bal, ok := r.localBalances[e.chanID]
	return bal, ok
}

// edgeProb returns the belief-adjusted success probability of pushing amt
// over an edge.
func (r *candidateRouter) edgeProb(e *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	k := dirKey{e.chanID, e.to}

	// Committed in-flight liquidity effectively raises the amount the
	// channel must bear.
	eff := amt + r.inFlight[k]

	// Our own channels have exact known balances.
	if bal, ok := r.localCap(e); ok {
		if eff <= bal {
			return knownOK
		}
		return 0
	}

	b := r.beliefs[k]
	if b != nil {
		if b.hasFail && eff >= b.failAmt {
			return 0
		}
		if eff <= b.okAmt {
			return knownOK
		}
	}

	p := bimodalPrior(eff, e.capacity)

	if b != nil && b.hasFail {
		// We know the channel dies somewhere in (okAmt, failAmt];
		// interpolate downward as we approach the failure bound.
		lo := float64(b.okAmt)
		hi := float64(b.failAmt)
		if hi > lo {
			frac := (float64(eff) - lo) / (hi - lo)
			p *= (1.0 - 0.85*frac)
		}
	}
	if b != nil && b.okAmt > 0 {
		// A prior success is strong bimodal evidence that most of
		// the capacity sits on our side.
		p = p + (1.0-p)*0.45
	}

	if n := r.nodeFail[e.to]; n > 0 {
		p *= math.Pow(0.55, float64(n))
	}

	if p < probFloor {
		p = probFloor
	}
	if p > probCeil {
		p = probCeil
	}
	return p
}

// pqItem is a search-frontier entry keyed by combined cost.
type pqItem struct {
	node     route.Vertex
	arriving lnwire.MilliSatoshi
	cost     float64
	prob     float64
	hops     int
	idx      int
}

type pq []*pqItem

func (q pq) Len() int           { return len(q) }
func (q pq) Less(i, j int) bool { return q[i].cost < q[j].cost }
func (q pq) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].idx = i
	q[j].idx = j
}
func (q *pq) Push(x any) {
	it := x.(*pqItem)
	it.idx = len(*q)
	*q = append(*q, it)
}
func (q *pq) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}

// searchState is the best known state at a node during backward search.
type searchState struct {
	arriving lnwire.MilliSatoshi
	cost     float64
	prob     float64
	hops     int
	edge     *candidateEdge
}

// findRoute runs a backward search from the target minimising
// fee + attemptCost * (1/prob - 1), i.e. expected total cost including
// the retries implied by an unreliable path. avoid excludes directed
// channels already claimed by another planned shard.
func (r *candidateRouter) findRoute(amt lnwire.MilliSatoshi,
	avoid map[dirKey]bool) (*route.Route, float64, error) {

	best := make(map[route.Vertex]*searchState)
	best[r.spec.Target] = &searchState{
		arriving: amt, cost: 0, prob: 1, hops: 0,
	}

	q := &pq{}
	heap.Push(q, &pqItem{
		node: r.spec.Target, arriving: amt, cost: 0, prob: 1,
	})

	for q.Len() > 0 {
		it := heap.Pop(q).(*pqItem)
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
			if avoid[dirKey{e.chanID, e.to}] {
				continue
			}
			if e.from != r.source && e.from == r.spec.Target {
				continue
			}

			amtOver := it.arriving
			if !e.policyOK(amtOver) {
				continue
			}

			p := r.edgeProb(e, amtOver)
			if p <= probFloor {
				continue
			}

			var sending lnwire.MilliSatoshi
			if e.from == r.source {
				sending = amtOver
			} else {
				sending = amtOver + r.hopFee(e, amtOver)
			}

			newProb := it.prob * p
			if newProb < 5e-4 {
				continue
			}

			feeCost := float64(sending - amt)
			riskCost := attemptCost * (1.0/newProb - 1.0)
			// Mild per-hop penalty: longer routes fail in more
			// ways than the model captures.
			hopCost := float64(it.hops+1) * 250.0
			total := feeCost + riskCost + hopCost

			prev := best[e.from]
			if prev != nil && total >= prev.cost-1e-9 {
				continue
			}

			best[e.from] = &searchState{
				arriving: sending,
				cost:     total,
				prob:     newProb,
				hops:     it.hops + 1,
				edge:     e,
			}
			heap.Push(q, &pqItem{
				node:     e.from,
				arriving: sending,
				cost:     total,
				prob:     newProb,
				hops:     it.hops + 1,
			})
		}
	}

	st := best[r.source]
	if st == nil {
		return nil, 0, errors.New("no route found")
	}

	rt, err := r.buildRoute(amt, best)
	if err != nil {
		return nil, 0, err
	}
	return rt, st.prob, nil
}

// hopFee is the fee we pay an intermediate hop, including a small
// headroom that grows if nodes have complained about fees. Headroom is
// zero by default so we do not overpay in the common case.
func (r *candidateRouter) hopFee(e *candidateEdge,
	amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	f := e.fee(amt)
	if r.feeBump > 0 {
		f += f*lnwire.MilliSatoshi(r.feeBump)/100 + 1
	}
	return f
}

// buildRoute walks the next-pointers from source to target.
func (r *candidateRouter) buildRoute(amt lnwire.MilliSatoshi,
	best map[route.Vertex]*searchState) (*route.Route, error) {

	var path []*candidateEdge
	node := r.source
	for node != r.spec.Target {
		st := best[node]
		if st == nil || st.edge == nil {
			return nil, errors.New("broken path")
		}
		path = append(path, st.edge)
		node = st.edge.to
		if len(path) > maxHops+1 {
			return nil, errors.New("path too long")
		}
	}
	if len(path) == 0 {
		return nil, errors.New("empty path")
	}

	n := len(path)
	amtOver := make([]lnwire.MilliSatoshi, n)
	expiry := make([]uint32, n)
	amtOver[n-1] = amt
	expiry[n-1] = finalCltvDelta

	for i := n - 2; i >= 0; i-- {
		fwd := path[i+1]
		amtOver[i] = amtOver[i+1] + r.hopFee(fwd, amtOver[i+1])
		expiry[i] = expiry[i+1] + uint32(fwd.timeLockDelta)
	}

	hops := make([]*route.Hop, n)
	for i, e := range path {
		amtFwd := amt
		out := uint32(finalCltvDelta)
		if i < n-1 {
			amtFwd = amtOver[i+1]
			out = expiry[i+1]
		}
		hops[i] = &route.Hop{
			PubKeyBytes:      e.to,
			ChannelID:        e.chanID,
			AmtToForward:     amtFwd,
			OutgoingTimeLock: out,
		}
	}

	return &route.Route{
		TotalTimeLock: expiry[0],
		TotalAmount:   amtOver[0],
		SourcePubKey:  r.source,
		Hops:          hops,
	}, nil
}

// routeCapacity returns the largest amount the given route could carry
// according to current beliefs and known local balances: the min over
// hops of usable liquidity minus what is already in flight.
func (r *candidateRouter) routeCapacity(rt *route.Route) lnwire.MilliSatoshi {
	limit := lnwire.MilliSatoshi(math.MaxUint32) * 1000

	for i, hop := range rt.Hops {
		k := dirKey{hop.ChannelID, hop.PubKeyBytes}
		e := r.findEdge(hop.ChannelID, hop.PubKeyBytes)
		if e == nil {
			continue
		}

		var avail lnwire.MilliSatoshi
		if bal, ok := r.localCap(e); ok {
			avail = bal
		} else {
			b := r.beliefs[k]
			avail = b.usable(e.capacity)
			if e.maxHTLC != 0 && e.maxHTLC < avail {
				avail = e.maxHTLC
			}
		}

		if used := r.inFlight[k]; used < avail {
			avail -= used
		} else {
			avail = 0
		}

		// Fees inflate upstream hops; discount the head of the
		// route slightly so the shard still fits after fees.
		if i == 0 && len(rt.Hops) > 1 {
			avail = avail * 995 / 1000
		}

		if avail < limit {
			limit = avail
		}
	}

	return limit
}

// findEdge looks up a directed edge by channel and destination.
func (r *candidateRouter) findEdge(chanID uint64,
	toward route.Vertex) *candidateEdge {

	for _, e := range r.incomingEdges[toward] {
		if e.chanID == chanID {
			return e
		}
	}
	return nil
}

// planShard chooses the next shard amount and its route jointly: it asks
// for a route at the full remaining amount first, and if the route's
// believed capacity cannot bear it, it re-plans at exactly the amount
// that corridor can carry rather than at a blind half.
func (r *candidateRouter) planShard(amt lnwire.MilliSatoshi,
	partsLeft uint32) (*route.Route, float64, error) {

	type cand struct {
		rt   *route.Route
		prob float64
		amt  lnwire.MilliSatoshi
	}

	var (
		bestFull *cand
		bestPart *cand
	)

	consider := func(sz lnwire.MilliSatoshi) {
		if sz < minShard && sz < amt {
			return
		}
		if sz > amt {
			sz = amt
		}
		rt, prob, err := r.findRoute(sz, nil)
		if err != nil {
			return
		}
		c := &cand{rt: rt, prob: prob, amt: sz}
		if sz == amt {
			if bestFull == nil || prob > bestFull.prob {
				bestFull = c
			}
			return
		}
		// Prefer the largest shard that still looks likely; break
		// ties by probability.
		if bestPart == nil ||
			prob*float64(sz) > bestPart.prob*float64(bestPart.amt) {

			bestPart = c
		}
	}

	// Step one: the whole remaining amount over the best corridor.
	consider(amt)

	// If the full-amount route already looks solid, take it: this is
	// the cheapest outcome in both fees and attempts.
	if bestFull != nil && bestFull.prob >= 0.55 {
		return bestFull.rt, bestFull.prob, nil
	}

	if partsLeft > 1 {
		// Step two: size a shard to what the best corridor is
		// believed to bear. This is the joint planning step: the
		// route tells us the bottleneck, and we cut the shard to
		// fit it exactly instead of halving.
		if bestFull != nil {
			capr := r.routeCapacity(bestFull.rt)
			if capr >= minShard && capr < amt {
				consider(capr)
			}
		}

		// Step three: a small ladder of deliberate fractions so a
		// large corridor can take most of the payment and smaller
		// ones the remainder. Scale the ladder with how many parts
		// remain and how much trouble we have had so far.
		fracs := []lnwire.MilliSatoshi{2, 3}
		if partsLeft >= 4 || r.consecFail >= 2 {
			fracs = append(fracs, 4, 6)
		}
		if partsLeft >= 6 || r.consecFail >= 4 {
			fracs = append(fracs, 10, 16)
		}
		for _, k := range fracs {
			consider(amt / k)
		}
		if amt/3 >= minShard {
			consider(amt * 2 / 3)
		}
	}

	// Choose between the full-amount route and the best partial one.
	// A part slot is a scarce resource, so demand that the partial be
	// meaningfully more likely before spending one.
	switch {
	case bestFull != nil && bestPart == nil:
		return bestFull.rt, bestFull.prob, nil

	case bestFull == nil && bestPart != nil:
		return bestPart.rt, bestPart.prob, nil

	case bestFull != nil && bestPart != nil:
		if bestPart.prob > bestFull.prob*1.6 &&
			bestPart.prob >= 0.30 {

			return bestPart.rt, bestPart.prob, nil
		}
		return bestFull.rt, bestFull.prob, nil
	}

	return nil, 0, errors.New("no route found")
}

// RequestRoute plans and commits the next shard.
//
// NOTE: Part of the routing.SimRouter interface.
func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	r.attempts++
	if r.attempts > maxAttempts {
		return nil, errors.New("attempt budget exhausted")
	}

	partsLeft := uint32(1)
	if r.spec.MaxParts > inFlightHtlcs {
		partsLeft = r.spec.MaxParts - inFlightHtlcs
	}

	rt, _, err := r.planShard(amt, partsLeft)
	if err == nil {
		r.commit(rt)
		return rt, nil
	}

	// Last resort: probe with progressively smaller shards, if we may
	// still split. Some corridor almost always takes a small amount.
	if partsLeft > 1 {
		for _, k := range []lnwire.MilliSatoshi{8, 16, 32, 64} {
			probe := amt / k
			if probe < minShard {
				break
			}
			if p, _, e := r.findRoute(probe, nil); e == nil {
				r.commit(p)
				return p, nil
			}
		}
	}

	return nil, errors.New("no route found")
}

// commit records the liquidity an in-flight route holds on each directed
// channel so parallel shards do not plan over the same corridor twice.
func (r *candidateRouter) commit(rt *route.Route) {
	for i, hop := range rt.Hops {
		amtOver := rt.TotalAmount
		if i > 0 {
			amtOver = rt.Hops[i-1].AmtToForward
		}
		k := dirKey{hop.ChannelID, hop.PubKeyBytes}
		r.inFlight[k] += amtOver
	}
}

// release undoes commit for a settled or failed route.
func (r *candidateRouter) release(rt *route.Route) {
	for i, hop := range rt.Hops {
		amtOver := rt.TotalAmount
		if i > 0 {
			amtOver = rt.Hops[i-1].AmtToForward
		}
		k := dirKey{hop.ChannelID, hop.PubKeyBytes}
		if cur := r.inFlight[k]; cur > amtOver {
			r.inFlight[k] = cur - amtOver
		} else {
			delete(r.inFlight, k)
		}
	}
}

// ReportAttempt folds attempt feedback into the liquidity beliefs: hops
// before the failure are proven to carry their amount, and the failing
// hop gets an upper-fail bound.
//
// NOTE: Part of the routing.SimRouter interface.
func (r *candidateRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	r.release(rt)

	amtAt := func(i int) lnwire.MilliSatoshi {
		if i == 0 {
			return rt.TotalAmount
		}
		return rt.Hops[i-1].AmtToForward
	}

	markOK := func(i int) {
		k := dirKey{rt.Hops[i].ChannelID, rt.Hops[i].PubKeyBytes}
		b := r.beliefs[k]
		if b == nil {
			b = &belief{}
			r.beliefs[k] = b
		}
		if a := amtAt(i); a > b.okAmt {
			b.okAmt = a
		}
		// A success invalidates any stale failure bound at or
		// below the amount we just pushed through.
		if b.hasFail && b.failAmt <= b.okAmt {
			b.hasFail = false
			b.failAmt = 0
		}
	}

	// Settled: every hop carried its amount.
	if result.Failure == nil {
		r.consecFail = 0
		for i := range rt.Hops {
			markOK(i)
		}
		if bal, ok := r.localBalances[rt.Hops[0].ChannelID]; ok {
			if bal > rt.TotalAmount {
				r.localBalances[rt.Hops[0].ChannelID] =
					bal - rt.TotalAmount
			} else {
				r.localBalances[rt.Hops[0].ChannelID] = 0
			}
		}
		return nil
	}

	r.consecFail++

	// Locate the failing hop index.
	failIdx := -1
	if result.FailureSource == rt.SourcePubKey {
		failIdx = 0
	}
	for i, hop := range rt.Hops {
		if hop.PubKeyBytes == result.FailureSource {
			failIdx = i + 1
		}
	}
	if failIdx < 0 {
		// Unknown source: nothing reliable to learn.
		return nil
	}

	// Everything strictly before the failing hop succeeded.
	for i := 0; i < failIdx && i < len(rt.Hops); i++ {
		markOK(i)
	}

	if failIdx >= len(rt.Hops) {
		// The final node itself rejected; nothing liquidity-wise
		// to learn about a channel.
		return nil
	}

	hop := rt.Hops[failIdx]
	k := dirKey{hop.ChannelID, hop.PubKeyBytes}

	switch result.Failure.(type) {
	case *lnwire.FailFeeInsufficient:
		// Our fee estimate was below the node's real policy: pay
		// a little more everywhere rather than abandoning the
		// corridor, which is often the only good one.
		if r.feeBump < 6 {
			r.feeBump += 3
		}
		r.nodeFail[hop.PubKeyBytes]++
		return nil

	case *lnwire.FailIncorrectCltvExpiry,
		*lnwire.FailExpiryTooSoon,
		*lnwire.FailChannelDisabled:

		// Policy mismatch, not liquidity: steer away from the node
		// rather than poisoning the channel's liquidity belief.
		r.nodeFail[hop.PubKeyBytes]++
		return nil
	}

	b := r.beliefs[k]
	if b == nil {
		b = &belief{}
		r.beliefs[k] = b
	}
	a := amtAt(failIdx)
	if !b.hasFail || a < b.failAmt {
		b.hasFail = true
		b.failAmt = a
	}
	if b.okAmt >= b.failAmt {
		// Contradiction from drift: trust the newer failure.
		b.okAmt = 0
	}

	// Our own channel failing means our balance estimate was wrong.
	if failIdx == 0 && a > 0 {
		r.localBalances[hop.ChannelID] = a - 1
	}

	return nil
}
