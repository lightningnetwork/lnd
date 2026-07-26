package main

// This file is the CANDIDATE SLOT for evolved routing algorithms.
//
// Design summary (changes over the previous candidate are marked NEW):
//   - Bimodal prior over amount/capacity for hidden liquidity, so wider
//     channels are preferred automatically at equal amounts.
//   - Per-DIRECTED-channel beliefs with hard lower-OK / upper-fail bounds
//     and no time decay: a stale bound costs one retry to refresh, which is
//     cheaper than decaying evidence.
//   - Balance bookkeeping on settle plus complementary reasoning across the
//     two sides of a channel (a dry reverse side means this side is full).
//   - NEW: PERSISTENT PARALLEL PLAN. The big observed failure was a large
//     payment (2.06 Gmsat) burning 20 attempts and dying on "no progress"
//     while every failure was a plain TemporaryChannelFailure. The root
//     cause was that the joint flow plan was thrown away on EVERY failure
//     (r.queued = nil), so the router collapsed back to single-corridor
//     probing and never actually held several unequal shards in flight.
//     Now the queue survives failures: only the shards whose corridors are
//     directly contradicted by new evidence are dropped, and the rest are
//     re-priced lazily when handed out.
//   - NEW: CONCURRENCY-FIRST DISPATCH. When the remainder cannot fit in a
//     single corridor and parts are free, we deliberately hand out the
//     largest believable shard immediately rather than searching the whole
//     amount ladder first. Filling MaxParts is what converts a big payment
//     into a success; ladder search only helps once concurrency is spent.
//   - NEW: RESIDUAL-AWARE FLOW PLANNING. planFlow now runs a proper
//     residual pass: each corridor's shard is capped by the min of its
//     bottleneck and the free local balance of its first hop, and local
//     first-hop budget is DECREMENTED as shards are planned, so two shards
//     never over-commit the same local channel. Previously two planned
//     shards could both be sized to the same local channel's balance.
//   - NEW: ADAPTIVE FAIL BUDGET. maxFailStreak scales with how much of the
//     payment still needs covering and how many parts we may use, so a
//     multi-shard payment is not killed by a streak that a single-shard
//     payment would deserve. Small payments still give up fast, so retry
//     efficiency on easy scenarios is preserved.
//   - NEW: PROGRESS-AWARE STREAK RESET. Any hop that demonstrably forwarded
//     (a failure deeper in the route than hop 0) counts as partial
//     progress and softens the streak, because we did learn something
//     actionable.
//   - Depth-aware lower retries with a per-direction dry-probe counter.
//   - Hopeless-payment detection from exactly known local balances.
//   - Duplicate-attempt suppression, and non-liquidity failures (fee, cltv,
//     min, disabled) repaired in the local policy view.

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	finalCltvDelta = 40

	// attemptCostBase and attemptCostPPM express the virtual cost of one
	// failed attempt. Success dominates the objective, so the retry cost
	// stays large relative to fees, but it is small enough that fee
	// differences break ties between similarly reliable corridors.
	attemptCostBase = lnwire.MilliSatoshi(1_200)
	attemptCostPPM  = lnwire.MilliSatoshi(6_000)

	minProb   = 0.005
	maxProb   = 0.985
	knownProb = 0.995

	// maxRouteHops bounds path length.
	maxRouteHops = 7

	// maxAttempts bounds total attempts for one payment.
	maxAttempts = 90

	// baseFailStreak is the streak budget for a payment we believe one
	// corridor should be able to carry. Payments needing many shards get
	// a proportionally larger budget, see failBudget.
	baseFailStreak = 10

	// maxFailStreakCap is the absolute ceiling on the streak budget.
	maxFailStreakCap = 34

	// probeBudget caps how many Dijkstra runs a single RequestRoute call
	// may spend per pass.
	probeBudget = 12

	// minShard is the smallest shard we will ever plan.
	minShard = lnwire.MilliSatoshi(1_000)

	// feeWeight and partCost shape the shard score: fee matters only as a
	// tie-break, while every extra expected part costs a little.
	feeWeight = 5.0
	partCost  = 0.004

	// priorSafeNum/priorSafeDen is the fraction of capacity we assume a
	// channel with no evidence can bear.
	priorSafeNum = 35
	priorSafeDen = 100

	// provenCenterNum/provenCenterDen is the fraction of capacity a
	// direction that has demonstrably forwarded is assumed to hold.
	provenCenterNum = 62
	provenCenterDen = 100

	// compSlackNum/compSlackDen discounts the complementary upper bound
	// derived from the reverse direction's proven liquidity.
	compSlackNum = 90
	compSlackDen = 100

	// revEmptyNum/revEmptyDen is the fraction of the liquidity implied on
	// this side by a reverse-direction failure that we bank on.
	revEmptyNum = 80
	revEmptyDen = 100

	// maxDryProbes is how many liquidity failures a single direction may
	// contribute before we stop probing it lower for this payment.
	maxDryProbes = 3

	// queueMinProb is the probability a queued shard must still clear
	// before we hand it out on a later call.
	queueMinProb = 0.12

	// flowRounds bounds the corridors a single flow decomposition walks.
	flowRounds = 14

	// hopelessStreak is how many consecutive failures we tolerate before
	// trusting the belief-derived local budget for a give-up decision.
	hopelessStreak = 6
)

// edgeKey identifies a directed channel: the channel plus the node the
// channel points at.
type edgeKey struct {
	chanID uint64
	to     route.Vertex
}

// edge is one directed channel of the public graph.
type edge struct {
	chanID   uint64
	from, to route.Vertex
	capacity lnwire.MilliSatoshi

	baseFee lnwire.MilliSatoshi
	feePPM  lnwire.MilliSatoshi
	cltv    uint16
	minHTLC lnwire.MilliSatoshi
	maxHTLC lnwire.MilliSatoshi
}

func (e *edge) key() edgeKey {
	return edgeKey{chanID: e.chanID, to: e.to}
}

// revKey is the key of the same channel in the opposite direction.
func (e *edge) revKey() edgeKey {
	return edgeKey{chanID: e.chanID, to: e.from}
}

func (e *edge) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return e.baseFee + amt*e.feePPM/1_000_000
}

func (e *edge) policyOK(amt lnwire.MilliSatoshi) bool {
	if amt < e.minHTLC {
		return false
	}
	if e.maxHTLC != 0 && amt > e.maxHTLC {
		return false
	}
	return amt <= e.capacity
}

// belief tracks what we have proven about a directed channel's liquidity.
type belief struct {
	// okAmt is the largest amount we currently believe is available, as
	// proven by a forward and adjusted for liquidity we have since moved.
	okAmt lnwire.MilliSatoshi

	// failAmt is the smallest amount believed to fail; hasFail guards it.
	failAmt lnwire.MilliSatoshi
	hasFail bool

	// succ records that this direction has actually forwarded for us.
	succ bool

	// fails counts liquidity failures charged to this direction.
	fails int

	// drained is how much we have pushed through this direction since the
	// evidence that set succ.
	drained lnwire.MilliSatoshi

	// inFlight is liquidity currently committed by our own HTLCs.
	inFlight lnwire.MilliSatoshi

	// misses counts failures we could not attribute to a specific hop but
	// that this channel took part in.
	misses int

	// dead marks a channel that failed permanently.
	dead bool
}

// bimodalPrior is the success probability of pushing amt through a channel
// of the given capacity with no direct evidence. Liquidity sits almost
// entirely on one side, so small amounts nearly always pass while amounts
// approaching capacity almost never do.
func bimodalPrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity == 0 {
		return minProb
	}
	if amt > capacity {
		return 0
	}
	x := float64(amt) / float64(capacity)

	// Decaying low mode: tiny fractions of capacity are near certain.
	low := math.Exp(-x * 3.2)

	// Logistic cliff as we approach capacity.
	cliff := 1.0 / (1.0 + math.Exp((x-0.42)*9.0))

	return clampProb(0.30*low + 0.70*cliff)
}

func clampProb(p float64) float64 {
	if p > maxProb {
		return maxProb
	}
	if p < minProb {
		return minProb
	}
	return p
}

// router is the evolved candidate.
type router struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	// inEdges maps a node to the directed edges arriving at it.
	inEdges map[route.Vertex][]*edge

	// byKey indexes every directed edge for failure attribution.
	byKey map[edgeKey]*edge

	// localEdges are the directed edges leaving our own node.
	localEdges []*edge

	localBalances map[uint64]lnwire.MilliSatoshi

	beliefs map[edgeKey]*belief

	// pending maps in-flight attempt ids to their routes.
	pending map[uint64]*route.Route

	// queued holds the remaining shards of a joint route-set plan. It is
	// NOT discarded on failure: shards whose corridors are contradicted by
	// new evidence are pruned individually and the rest are re-priced when
	// handed out.
	queued []*plan

	// failedSigs remembers (path, amount) pairs that already failed so we
	// never hand out the identical attempt twice.
	failedSigs map[string]bool

	// failStreak counts consecutive unproductive failures.
	failStreak int

	// attempts counts every route we handed out.
	attempts int

	// delivered is how much this payment has settled so far, used to size
	// the fail budget.
	delivered lnwire.MilliSatoshi

	// firstAmt is the amount of the very first RequestRoute call, i.e. the
	// full payment amount.
	firstAmt lnwire.MilliSatoshi
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	r := &router{
		source:        source,
		spec:          spec,
		inEdges:       make(map[route.Vertex][]*edge),
		byKey:         make(map[edgeKey]*edge),
		localBalances: localBalances,
		beliefs:       make(map[edgeKey]*belief),
		pending:       make(map[uint64]*route.Route),
		failedSigs:    make(map[string]bool),
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

				e := &edge{
					chanID: ch.ChannelID,
					from:   ch.OtherNode,
					to:     node,
					capacity: lnwire.NewMSatFromSatoshis(
						ch.Capacity,
					),
					baseFee: pol.FeeBaseMSat,
					feePPM: pol.
						FeeProportionalMillionths,
					cltv:    pol.TimeLockDelta,
					minHTLC: pol.MinHTLC,
				}
				if pol.HasMaxHTLC {
					e.maxHTLC = pol.MaxHTLC
				}

				r.inEdges[e.to] = append(r.inEdges[e.to], e)
				r.byKey[e.key()] = e
				if e.from == source {
					r.localEdges = append(
						r.localEdges, e,
					)
				}

				return nil
			}, func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	return r, nil
}

// mppOK reports whether this payment may be split at all.
func (r *router) mppOK() bool {
	return r.spec.MaxParts > 1
}

// bel returns (creating if needed) the belief for a directed channel.
func (r *router) bel(k edgeKey) *belief {
	b, ok := r.beliefs[k]
	if !ok {
		b = &belief{}
		r.beliefs[k] = b
	}
	return b
}

// view returns a read-only belief for a directed channel, substituting a
// zero-value belief when we have no evidence at all.
func (r *router) view(k edgeKey) *belief {
	if b, ok := r.beliefs[k]; ok {
		return b
	}
	return &belief{}
}

// capOf is the capacity of a directed channel, or zero when we do not know
// the direction at all.
func (r *router) capOf(k edgeKey) lnwire.MilliSatoshi {
	if e, ok := r.byKey[k]; ok {
		return e.capacity
	}
	return 0
}

// failBudget is how many consecutive failures we tolerate. A payment that
// provably needs several shards deserves more probing than one that should
// fit in a single corridor: each shard costs at least one attempt to place,
// and a shard that fails costs one more to resize. Small payments keep a
// tight budget so retry efficiency on easy scenarios does not regress.
func (r *router) failBudget() int {
	budget := baseFailStreak
	if !r.mppOK() {
		return budget
	}

	// Estimate the number of shards the payment needs from the largest
	// single local channel we could push out of.
	shards := 1
	if single := r.rawMaxLocal(); single > 0 && r.firstAmt > single {
		shards = int(r.firstAmt/single) + 1
	}
	if mp := int(r.spec.MaxParts); shards > mp && mp > 0 {
		shards = mp
	}
	if shards > 1 {
		budget += 5 * (shards - 1)
	}
	if budget > maxFailStreakCap {
		budget = maxFailStreakCap
	}
	return budget
}

// retryLimit is how far we are still willing to push a direction that has
// proven a failure. A failure at a tiny fraction of capacity means the
// direction is essentially empty, while a failure near capacity leaves a lot
// of plausible room underneath. After several dry probes we stop entirely.
func retryLimit(b *belief, capacity lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	if !b.hasFail {
		return 0
	}
	if b.fails >= maxDryProbes {
		return b.okAmt
	}
	depth := 0.5
	if capacity > 0 {
		depth = float64(b.failAmt) / float64(capacity)
		if depth > 1 {
			depth = 1
		}
	}
	f := 0.40 + 0.35*depth
	lim := lnwire.MilliSatoshi(float64(b.failAmt) * f)
	if lim < b.okAmt {
		lim = b.okAmt
	}
	return lim
}

// compBound is the complementary upper bound on a direction: liquidity we
// have proven sits on the reverse side cannot also sit on this side.
func (r *router) compBound(e *edge) lnwire.MilliSatoshi {
	rb := r.beliefs[e.revKey()]
	if rb == nil || rb.okAmt == 0 {
		return e.capacity
	}
	held := rb.okAmt * compSlackNum / compSlackDen
	if held >= e.capacity {
		return 0
	}
	return e.capacity - held
}

// provenCenter is the amount we believe this direction actually holds.
func (r *router) provenCenter(e *edge) lnwire.MilliSatoshi {
	var center lnwire.MilliSatoshi

	b := r.beliefs[e.key()]
	if b != nil && b.succ {
		c := e.capacity * provenCenterNum / provenCenterDen
		if c > b.drained {
			center = c - b.drained
		}
	}

	// Bimodal complement: the reverse side came up dry at failAmt, so this
	// side is holding close to the whole channel.
	if rb := r.beliefs[e.revKey()]; rb != nil && rb.hasFail {
		if e.capacity > rb.failAmt {
			inf := (e.capacity - rb.failAmt) *
				revEmptyNum / revEmptyDen
			if inf > center {
				center = inf
			}
		}
	}

	if b != nil && b.okAmt > center {
		center = b.okAmt
	}

	return center
}

// availCap is the hard upper bound on what we are still willing to push
// over an edge. It returns zero when the edge is unusable at any amount.
func (r *router) availCap(e *edge) lnwire.MilliSatoshi {
	c := e.capacity
	if e.maxHTLC != 0 && e.maxHTLC < c {
		c = e.maxHTLC
	}

	b := r.beliefs[e.key()]
	if b != nil && b.dead {
		return 0
	}

	if e.from == r.source {
		// Our own balance is known exactly.
		if bal := r.localBalances[e.chanID]; bal < c {
			c = bal
		}
	} else {
		if comp := r.compBound(e); comp < c {
			c = comp
		}
		if b != nil && b.hasFail {
			// Retry below the proven failure point rather than
			// blacklisting the channel outright.
			if lim := retryLimit(b, e.capacity); lim < c {
				c = lim
			}
		}
	}

	if b != nil {
		if b.inFlight >= c {
			return 0
		}
		c -= b.inFlight
	}

	return c
}

// safeCap is the largest amount we believe this edge can actually bear with
// decent probability. It is the sizing primitive for shard planning.
func (r *router) safeCap(e *edge) lnwire.MilliSatoshi {
	hard := r.availCap(e)
	if hard == 0 {
		return 0
	}
	if e.from == r.source {
		return hard
	}

	b := r.view(e.key())
	est := e.capacity * priorSafeNum / priorSafeDen
	if b.okAmt > est {
		est = b.okAmt
	}
	if !b.hasFail {
		// A proven forward, or a dry reverse direction, says most of
		// the channel funds sit on this side.
		if c := r.provenCenter(e) * 85 / 100; c > est {
			est = c
		}
	} else if lim := retryLimit(b, e.capacity); est > lim {
		est = lim
	}
	if b.misses > 0 {
		est = est / lnwire.MilliSatoshi(1+b.misses)
	}
	if est > hard {
		est = hard
	}
	return est
}

// prob is our success probability estimate for sending amt over the edge.
func (r *router) prob(e *edge, amt lnwire.MilliSatoshi) float64 {
	if amt == 0 {
		return 1
	}

	b := r.view(e.key())
	if b.dead {
		return 0
	}

	eff := amt + b.inFlight

	// Our own channels have exactly known balances.
	if e.from == r.source {
		if r.localBalances[e.chanID] >= eff {
			return knownProb
		}
		return 0
	}

	// The complementary bound is hard evidence: that much liquidity is
	// provably parked on the other side of the channel.
	if comp := r.compBound(e); eff > comp {
		return minProb / 4
	}

	var p float64
	switch {
	case b.okAmt >= eff:
		p = knownProb

	case b.hasFail && eff >= b.failAmt:
		return minProb / 4

	default:
		p = bimodalPrior(eff, e.capacity)
		center := r.provenCenter(e)

		switch {
		case b.okAmt > 0 && b.hasFail:
			// Known-good below and known-bad above compress the
			// uncertain interval.
			lo := float64(b.okAmt)
			hi := float64(b.failAmt)
			if hi > lo {
				frac := (float64(eff) - lo) / (hi - lo)
				p = 0.5*p + 0.5*(1-frac)
			}

		case center > 0 && !b.hasFail:
			// Bimodal optimism: the funds are believed to sit on
			// this side, less whatever we have drained since.
			q := 1.0 / (1.0 + math.Exp(
				(float64(eff)/float64(center)-1)*4.5,
			))
			p = 0.3*p + 0.7*q

		case b.hasFail:
			// Under bimodality a failure means the direction is
			// probably empty, so retries below it are a last
			// resort, not a coin flip.
			frac := float64(eff) / float64(b.failAmt)
			p = 0.5 * p * (1 - frac)
		}
	}

	if b.misses > 0 {
		m := b.misses
		if m > 3 {
			m = 3
		}
		p *= math.Pow(0.6, float64(m))
	}

	return clampProb(p)
}

// --- Dijkstra -------------------------------------------------------------

type pqItem struct {
	node route.Vertex
	dist float64
	amt  lnwire.MilliSatoshi
	// logProb is the accumulated ln(probability) from node to target.
	logProb float64
	hops    int
	idx     int
}

type pq []*pqItem

func (q pq) Len() int           { return len(q) }
func (q pq) Less(i, j int) bool { return q[i].dist < q[j].dist }
func (q pq) Swap(i, j int)      { q[i], q[j] = q[j], q[i]; q[i].idx = i; q[j].idx = j }
func (q *pq) Push(x any)        { it := x.(*pqItem); it.idx = len(*q); *q = append(*q, it) }
func (q *pq) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}

// pathState is the best known way to reach the target from a node.
type pathState struct {
	dist float64
	via  *edge
}

// findPath runs a probability-weighted backward Dijkstra for a delivered
// amount and returns the edge sequence from source to target. The cost of a
// path is its fee plus an attempt cost divided by the path's success
// probability, which is the standard risk/fee trade-off.
//
// avoid names channels that must not be reused, so parallel shards do not
// contend for the same liquidity.
func (r *router) findPath(amt lnwire.MilliSatoshi,
	avoid map[uint64]bool) ([]*edge, error) {

	if amt == 0 {
		return nil, errors.New("zero amount")
	}

	attemptCost := float64(attemptCostBase + amt*attemptCostPPM/1_000_000)
	hopCost := attemptCost * 0.2

	best := make(map[route.Vertex]*pathState)
	target := r.spec.Target
	best[target] = &pathState{dist: 0}

	q := &pq{}
	heap.Push(q, &pqItem{node: target, dist: 0, amt: amt})

	for q.Len() > 0 {
		it := heap.Pop(q).(*pqItem)
		cur := best[it.node]
		if cur == nil || it.dist > cur.dist+1e-9 {
			continue
		}
		if it.node == r.source {
			break
		}
		if it.hops >= maxRouteHops {
			continue
		}

		for _, e := range r.inEdges[it.node] {
			if avoid[e.chanID] || e.from == it.node {
				continue
			}

			amtOver := it.amt
			if !e.policyOK(amtOver) {
				continue
			}
			if amtOver > r.availCap(e) {
				continue
			}

			p := r.prob(e, amtOver)
			if p <= 0 {
				continue
			}

			sending := amtOver
			if e.from != r.source {
				sending += e.fee(amtOver)
			}

			logProb := it.logProb + math.Log(p)
			totalProb := math.Exp(logProb)
			if totalProb < 1e-5 {
				continue
			}

			fee := float64(sending) - float64(amt)
			dist := (fee+attemptCost)/totalProb +
				float64(it.hops+1)*hopCost

			if prev, ok := best[e.from]; ok &&
				dist >= prev.dist-1e-9 {

				continue
			}

			best[e.from] = &pathState{dist: dist, via: e}

			heap.Push(q, &pqItem{
				node:    e.from,
				dist:    dist,
				amt:     sending,
				logProb: logProb,
				hops:    it.hops + 1,
			})
		}
	}

	src, ok := best[r.source]
	if !ok || src.via == nil {
		return nil, errors.New("no route found")
	}

	var path []*edge
	node := r.source
	for node != r.spec.Target {
		st, ok := best[node]
		if !ok || st.via == nil {
			return nil, errors.New("broken path")
		}
		path = append(path, st.via)
		node = st.via.to
		if len(path) > maxRouteHops {
			return nil, errors.New("path too long")
		}
	}
	if len(path) == 0 {
		return nil, errors.New("empty path")
	}

	return path, nil
}

// makeRoute prices a known path at a delivered amount, validating every
// hop's policy and belief bounds. It returns the route and its estimated
// success probability.
func (r *router) makeRoute(path []*edge,
	amt lnwire.MilliSatoshi) (*route.Route, float64, error) {

	if amt == 0 {
		return nil, 0, errors.New("zero amount")
	}

	n := len(path)
	amtOver := make([]lnwire.MilliSatoshi, n)
	expiry := make([]uint32, n)

	amtOver[n-1] = amt
	expiry[n-1] = finalCltvDelta

	for i := n - 2; i >= 0; i-- {
		fwd := path[i+1]
		amtOver[i] = amtOver[i+1] + fwd.fee(amtOver[i+1])
		expiry[i] = expiry[i+1] + uint32(fwd.cltv)
	}

	logProb := 0.0
	for i, e := range path {
		if !e.policyOK(amtOver[i]) {
			return nil, 0, errors.New("policy violated")
		}
		if amtOver[i] > r.availCap(e) {
			return nil, 0, errors.New("above believed capacity")
		}
		p := r.prob(e, amtOver[i])
		if p <= 0 {
			return nil, 0, errors.New("hopeless hop")
		}
		logProb += math.Log(p)
	}

	hops := make([]*route.Hop, n)
	for i, e := range path {
		amtFwd := amt
		outExpiry := uint32(finalCltvDelta)
		if i < n-1 {
			amtFwd = amtOver[i+1]
			outExpiry = expiry[i+1]
		}
		hops[i] = &route.Hop{
			PubKeyBytes:      e.to,
			ChannelID:        e.chanID,
			AmtToForward:     amtFwd,
			OutgoingTimeLock: outExpiry,
		}
	}

	rt := &route.Route{
		TotalTimeLock: expiry[0],
		TotalAmount:   amtOver[0],
		SourcePubKey:  r.source,
		Hops:          hops,
	}

	return rt, math.Exp(logProb), nil
}

// --- Shard planning -------------------------------------------------------

// localBudget is the total liquidity we believe is spendable out of our own
// channels right now, which upper-bounds the whole payment.
func (r *router) localBudget() lnwire.MilliSatoshi {
	var total lnwire.MilliSatoshi
	seen := make(map[uint64]bool)
	for _, e := range r.localEdges {
		if seen[e.chanID] {
			continue
		}
		seen[e.chanID] = true
		total += r.availCap(e)
	}
	return total
}

// rawLocalBudget is the total balance across our own channels, ignoring all
// beliefs and policies. It is a hard ceiling on anything we can deliver.
func (r *router) rawLocalBudget() lnwire.MilliSatoshi {
	var total lnwire.MilliSatoshi
	for _, bal := range r.localBalances {
		total += bal
	}
	return total
}

// rawMaxLocal is the largest single local channel balance, which bounds one
// shard: a shard leaves through exactly one first hop.
func (r *router) rawMaxLocal() lnwire.MilliSatoshi {
	var best lnwire.MilliSatoshi
	for _, bal := range r.localBalances {
		if bal > best {
			best = bal
		}
	}
	return best
}

// maxLocalEdge is the largest single local channel balance still free, which
// is the true ceiling on one shard.
func (r *router) maxLocalEdge() lnwire.MilliSatoshi {
	var best lnwire.MilliSatoshi
	seen := make(map[uint64]bool)
	for _, e := range r.localEdges {
		if seen[e.chanID] {
			continue
		}
		seen[e.chanID] = true
		if c := r.availCap(e); c > best {
			best = c
		}
	}
	return best
}

// freeLocal is the free capacity of the local channel a corridor leaves
// through, or a very large number when the corridor's first hop is not one
// of ours (which cannot happen for a valid route).
func (r *router) freeLocal(path []*edge) lnwire.MilliSatoshi {
	if len(path) == 0 {
		return 0
	}
	return r.availCap(path[0])
}

// bottleneck is the delivered amount a corridor is believed able to bear,
// derived from each hop's safe capacity with a small margin for fees.
func (r *router) bottleneck(path []*edge) lnwire.MilliSatoshi {
	bn := lnwire.MilliSatoshi(math.MaxUint32) * 1024
	for _, e := range path {
		c := r.safeCap(e)
		if c == 0 {
			return 0
		}
		if c < bn {
			bn = c
		}
	}
	return bn - bn/100
}

// ladder builds a descending set of candidate shard sizes. It mixes even
// splits over the parts we can still afford, a geometric descent that
// always reaches genuinely small amounts, exact local channel balances, and
// evidence-derived sizes just below proven failure points.
func (r *router) ladder(hi, remaining lnwire.MilliSatoshi,
	partsLeft uint32) []lnwire.MilliSatoshi {

	set := make(map[lnwire.MilliSatoshi]bool)
	var out []lnwire.MilliSatoshi
	add := func(a lnwire.MilliSatoshi) {
		if a < minShard || a > hi || set[a] {
			return
		}
		set[a] = true
		out = append(out, a)
	}

	add(hi)

	// Even splits over the part counts we could still afford.
	maxK := partsLeft
	if maxK < 4 && r.mppOK() {
		maxK = 4
	}
	if maxK > 10 {
		maxK = 10
	}
	for k := uint32(2); k <= maxK; k++ {
		add(remaining / lnwire.MilliSatoshi(k))
	}

	// Geometric descent all the way down to a small floor.
	floor := hi / 4096
	if floor < minShard {
		floor = minShard
	}
	cur := hi
	for i := 0; i < 16; i++ {
		cur = cur / 2
		if cur < floor {
			break
		}
		add(cur)
	}

	// Local channel balances are exact knowledge, and a shard sized to a
	// local channel is exactly what a fan-out split wants.
	seen := make(map[uint64]bool)
	for _, e := range r.localEdges {
		if seen[e.chanID] {
			continue
		}
		seen[e.chanID] = true
		if c := r.availCap(e); c >= minShard {
			add(c - c/200)
		}
	}

	// Evidence-derived sizes: believed-available amounts and just under
	// proven failure points.
	for k, b := range r.beliefs {
		if b.dead {
			continue
		}
		if b.okAmt >= minShard {
			add(b.okAmt)
		}
		if b.hasFail {
			if lim := retryLimit(b, r.capOf(k)); lim >= minShard {
				add(lim)
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

// inFlightChans returns the channels currently carrying our own HTLCs.
func (r *router) inFlightChans() map[uint64]bool {
	m := make(map[uint64]bool)
	for _, rt := range r.pending {
		for _, h := range rt.Hops {
			m[h.ChannelID] = true
		}
	}
	return m
}

// score rates a candidate shard: probability-weighted delivered value, with
// a bonus for finishing the payment outright, a light fee penalty, and a
// cost for every additional part the split implies.
func (r *router) score(a lnwire.MilliSatoshi, p float64,
	fee lnwire.MilliSatoshi, remaining lnwire.MilliSatoshi) float64 {

	v := p * float64(a)
	if a >= remaining {
		v *= 1.15
	}
	v -= feeWeight * float64(fee)

	parts := float64(remaining) / float64(a)
	if parts > 1 {
		v -= partCost * float64(remaining) * (parts - 1)
	}
	return v
}

// planSig identifies a (path, delivered amount) attempt.
func planSig(path []*edge, amt lnwire.MilliSatoshi) string {
	buf := make([]byte, 0, len(path)*14+14)
	for _, e := range path {
		buf = strconv.AppendUint(buf, e.chanID, 36)
		buf = append(buf, ':')
	}
	buf = strconv.AppendUint(buf, uint64(amt)/1024, 36)
	return string(buf)
}

// routeSig is planSig for an already-built route.
func routeSig(rt *route.Route) string {
	n := len(rt.Hops)
	if n == 0 {
		return ""
	}
	buf := make([]byte, 0, n*14+14)
	for _, h := range rt.Hops {
		buf = strconv.AppendUint(buf, h.ChannelID, 36)
		buf = append(buf, ':')
	}
	buf = strconv.AppendUint(
		buf, uint64(rt.Hops[n-1].AmtToForward)/1024, 36,
	)
	return string(buf)
}

type plan struct {
	path  []*edge
	rt    *route.Route
	score float64
	amt   lnwire.MilliSatoshi
	prob  float64
}

// evalPath scores a corridor at the requested amount and at the amount the
// corridor is believed able to bear, keeping the better of the two.
func (r *router) evalPath(path []*edge, a, remaining lnwire.MilliSatoshi,
	best *plan) *plan {

	cands := []lnwire.MilliSatoshi{a}
	if bn := r.bottleneck(path); bn >= minShard {
		g := bn
		if g > remaining {
			g = remaining
		}
		if g != a {
			cands = append(cands, g)
		}
	}

	for _, c := range cands {
		if c < minShard {
			continue
		}
		if r.failedSigs[planSig(path, c)] {
			continue
		}
		rt, p, err := r.makeRoute(path, c)
		if err != nil {
			continue
		}
		fee := rt.TotalAmount - c
		s := r.score(c, p, fee, remaining)
		if best == nil || s > best.score {
			best = &plan{
				path: path, rt: rt, score: s, amt: c, prob: p,
			}
		}
	}

	return best
}

// bestOnPath prices a corridor at descending amounts and returns the best
// novel attempt it can still carry.
func (r *router) bestOnPath(path []*edge,
	hi, remaining lnwire.MilliSatoshi) *plan {

	a := hi
	for i := 0; i < 14 && a >= minShard; i++ {
		if !r.failedSigs[planSig(path, a)] {
			rt, p, err := r.makeRoute(path, a)
			if err == nil {
				fee := rt.TotalAmount - a
				return &plan{
					path:  path,
					rt:    rt,
					amt:   a,
					prob:  p,
					score: r.score(a, p, fee, remaining),
				}
			}
		}
		a = a * 3 / 4
	}
	return nil
}

// findAnyPath looks for a corridor able to carry hi, falling back down the
// amount ladder when nothing can take the full amount.
func (r *router) findAnyPath(hi, remaining lnwire.MilliSatoshi,
	partsLeft uint32, avoid map[uint64]bool) []*edge {

	if p, err := r.findPath(hi, avoid); err == nil {
		return p
	}

	probes := 0
	for _, a := range r.ladder(hi, remaining, partsLeft) {
		if a >= hi {
			continue
		}
		if probes >= 7 {
			break
		}
		probes++
		if p, err := r.findPath(a, avoid); err == nil {
			return p
		}
	}
	return nil
}

// planFlow decomposes the remaining amount over several DISJOINT corridors,
// sizing each shard to what that corridor's weakest hop is believed able to
// bear. This is the min-cost-flow style joint plan: unequal parallel
// corridors each get a shard that fits, instead of discovering the split by
// failing at a blind half.
//
// It runs a residual pass: local first-hop budget is decremented as shards
// are planned, so two shards can never over-commit the same local channel,
// and each shard is capped by the min of its corridor bottleneck and its
// first hop's remaining free balance.
func (r *router) planFlow(remaining lnwire.MilliSatoshi, partsLeft uint32,
	busy map[uint64]bool) []*plan {

	avoid := make(map[uint64]bool, len(busy)+8)
	for c := range busy {
		avoid[c] = true
	}

	// Residual local budget per first-hop channel.
	residual := make(map[uint64]lnwire.MilliSatoshi, len(r.localEdges))
	for _, e := range r.localEdges {
		if _, ok := residual[e.chanID]; !ok {
			residual[e.chanID] = r.availCap(e)
		}
	}

	var out []*plan
	left := remaining

	// Queued shards are handed out on later calls, as concurrency frees
	// up, so the decomposition is not bounded by the parts free right now.
	rounds := 1
	if r.mppOK() {
		rounds = flowRounds
	}

	for k := 0; k < rounds; k++ {
		if left < minShard {
			break
		}

		// The largest amount any still-usable local channel can push.
		var single lnwire.MilliSatoshi
		for _, c := range residual {
			if c > single {
				single = c
			}
		}
		if single < minShard {
			break
		}

		want := left
		if want > single {
			want = single
		}

		path := r.findAnyPath(want, remaining, partsLeft, avoid)
		if path == nil {
			break
		}

		amtS := want
		if fl := residual[path[0].chanID]; fl > 0 && fl < amtS {
			amtS = fl
		}
		if bn := r.bottleneck(path); bn < amtS {
			amtS = bn
		}
		if amtS < minShard {
			// This corridor cannot bear a useful shard, so retire
			// its first hop for this decomposition.
			avoid[path[0].chanID] = true
			residual[path[0].chanID] = 0
			continue
		}

		pl := r.bestOnPath(path, amtS, remaining)
		if pl == nil {
			avoid[path[0].chanID] = true
			continue
		}

		out = append(out, pl)

		// Charge the shard against its first hop's residual balance and
		// retire every channel it uses from this decomposition.
		fh := path[0].chanID
		if residual[fh] > pl.amt {
			residual[fh] -= pl.amt
		} else {
			residual[fh] = 0
		}
		for _, e := range path {
			avoid[e.chanID] = true
		}

		if pl.amt >= left {
			break
		}
		left -= pl.amt
	}

	return out
}

// flowTotal is the amount a plan set delivers in aggregate.
func flowTotal(plans []*plan) lnwire.MilliSatoshi {
	var total lnwire.MilliSatoshi
	for _, pl := range plans {
		total += pl.amt
	}
	return total
}

// pathBusy reports whether a corridor touches a channel already carrying one
// of our in-flight shards.
func pathBusy(path []*edge, busy map[uint64]bool) bool {
	for _, e := range path {
		if busy[e.chanID] {
			return true
		}
	}
	return false
}

// pathUses reports whether a corridor traverses the given channel.
func pathUses(path []*edge, chanID uint64) bool {
	for _, e := range path {
		if e.chanID == chanID {
			return true
		}
	}
	return false
}

// pruneQueue drops the queued shards that a fresh failure on chanID
// contradicts, and keeps the rest. Holding on to the plan across failures is
// what lets a multi-shard payment actually fill its concurrency instead of
// collapsing back to single-corridor probing after every miss.
func (r *router) pruneQueue(chanID uint64) {
	if len(r.queued) == 0 {
		return
	}
	kept := r.queued[:0]
	for _, pl := range r.queued {
		if pathUses(pl.path, chanID) {
			continue
		}
		kept = append(kept, pl)
	}
	r.queued = kept
}

// hopeless reports whether the remainder is provably beyond what our own
// channels can still push.
func (r *router) hopeless(amt lnwire.MilliSatoshi, inFlight uint32) bool {
	if inFlight > 0 {
		return false
	}
	if len(r.localBalances) == 0 {
		return false
	}

	// Hard ceiling: total outbound balance.
	if r.rawLocalBudget() < amt {
		return true
	}

	// A payment that may not be split must fit through one channel.
	if !r.mppOK() && r.rawMaxLocal() < amt {
		return true
	}

	// Softer test, only once we have actually confirmed dryness by
	// failing: the believed-free local liquidity cannot cover the rest.
	if r.failStreak >= hopelessStreak && len(r.localEdges) > 0 &&
		r.localBudget() < amt {

		return true
	}

	return false
}

// serveQueued hands out the first still-valid shard of a joint plan. Shards
// are re-priced against current beliefs, and a shard whose corridor no
// longer holds up is dropped rather than aborting the whole plan.
func (r *router) serveQueued(amt lnwire.MilliSatoshi, split bool,
	busy map[uint64]bool) *plan {

	var deferred []*plan

	for len(r.queued) > 0 {
		pl := r.queued[0]
		r.queued = r.queued[1:]

		a := pl.amt
		if a > amt {
			a = amt
		}
		if a < minShard {
			continue
		}
		// A partial shard is useless when the payment cannot be split.
		if !split && a < amt {
			r.queued = nil
			break
		}
		if pathBusy(pl.path, busy) {
			// The corridor is temporarily occupied, not wrong:
			// keep it for a later call.
			deferred = append(deferred, pl)
			continue
		}
		if r.failedSigs[planSig(pl.path, a)] {
			// Re-price the corridor a little lower rather than
			// discarding a corridor we deliberately chose.
			if lower := r.bestOnPath(pl.path, a-a/8, amt); lower !=
				nil && lower.prob >= queueMinProb {

				r.queued = append(deferred, r.queued...)
				return lower
			}
			continue
		}
		rt, p, err := r.makeRoute(pl.path, a)
		if err != nil || p < queueMinProb {
			// Beliefs moved against this corridor: try a smaller
			// amount on it before giving it up.
			if lower := r.bestOnPath(pl.path, a*3/4, amt); lower !=
				nil && lower.prob >= queueMinProb {

				r.queued = append(deferred, r.queued...)
				return lower
			}
			continue
		}

		r.queued = append(deferred, r.queued...)
		return &plan{path: pl.path, rt: rt, amt: a, prob: p}
	}

	r.queued = deferred
	return nil
}

// RequestRoute plans the next shard.
//
// Order of play:
//  1. Serve a shard left over from a joint route-set plan, re-priced.
//  2. When the remainder provably cannot fit through one local channel and
//     parts are still free, plan the whole decomposition up front and hand
//     out its first shard immediately. Filling concurrency with correctly
//     sized unequal shards is what turns a large payment into a success.
//  3. Otherwise search jointly over shard amount and corridor.
//  4. Fall back to a deliberate multi-corridor split, then to salvage.
//
// NOTE: Part of the routing.SimRouter interface.
func (r *router) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt == 0 {
		return nil, errors.New("zero amount")
	}
	if r.firstAmt == 0 {
		r.firstAmt = amt
	}
	if r.attempts >= maxAttempts {
		return nil, errors.New("attempt budget exhausted")
	}
	if r.failStreak >= r.failBudget() {
		return nil, errors.New("no progress")
	}
	if r.hopeless(amt, inFlightHtlcs) {
		return nil, errors.New("remainder exceeds local liquidity")
	}

	partsLeft := uint32(1)
	if r.spec.MaxParts > inFlightHtlcs {
		partsLeft = r.spec.MaxParts - inFlightHtlcs
	}

	busy := r.inFlightChans()
	split := r.mppOK()

	// Step 1: serve a queued shard from an earlier joint plan.
	if pl := r.serveQueued(amt, split, busy); pl != nil {
		r.attempts++
		return pl.rt, nil
	}

	// A single shard can never exceed what one local channel can push,
	// since a shard leaves through exactly one first hop.
	single := r.maxLocalEdge()
	hi := amt
	if single > 0 && single < hi {
		hi = single
	}
	if hi < minShard {
		hi = amt
	}

	// Step 2: concurrency-first dispatch. When the remainder provably
	// exceeds any one local channel, ladder search over a single corridor
	// cannot succeed, so plan the whole decomposition now.
	if split && partsLeft > 1 && single > 0 && amt > single {
		flow := r.planFlow(amt, partsLeft, busy)
		if len(flow) > 0 {
			r.queued = flow[1:]
			r.attempts++
			return flow[0].rt, nil
		}
	}

	ladder := r.ladder(hi, amt, partsLeft)

	var best *plan

	// First pass prefers corridors disjoint from in-flight shards so
	// parallel parts do not fight over the same liquidity.
	for pass := 0; pass < 2; pass++ {
		var avoid map[uint64]bool
		if pass == 0 {
			if len(busy) == 0 {
				continue
			}
			avoid = busy
		}

		probes := 0
		for _, a := range ladder {
			// Once we hold a solid plan, digging far below it only
			// wastes attempts and parts.
			if best != nil && best.prob >= 0.6 &&
				a < best.amt/2 {

				break
			}
			if probes >= probeBudget {
				break
			}
			if !split && a < amt {
				break
			}

			path, err := r.findPath(a, avoid)
			probes++
			if err != nil {
				continue
			}

			best = r.evalPath(path, a, amt, best)

			// A confident full-amount plan needs no alternatives.
			if best != nil && best.amt >= amt && best.prob > 0.8 {
				break
			}
		}

		if best != nil {
			break
		}
	}

	// Step 4: when no single corridor carries the whole remainder, plan the
	// split deliberately over disjoint corridors instead of halving
	// blindly. We compare the aggregate believed flow of the plan set
	// against the best single shard: covering more of the payment beats a
	// slightly nicer first hop, because every uncovered millisat is a
	// failed payment.
	if split && (best == nil || best.amt < amt) {
		flow := r.planFlow(amt, partsLeft, busy)
		if len(flow) > 0 {
			total := flowTotal(flow)
			first := flow[0]

			if best == nil || total > best.amt {
				r.queued = flow[1:]
				best = first
			}
		}
	}

	// Last resort: deliver whatever we can. Even a small settled shard
	// reduces the remainder and refreshes evidence, which is strictly
	// better than terminally giving up on the payment.
	if best == nil {
		best = r.salvage(amt, busy, split)
	}

	if best == nil {
		return nil, errors.New("no route found")
	}

	r.attempts++
	return best.rt, nil
}

// salvage hunts for any novel attempt at all, walking a wide descending
// amount ladder over both the disjoint and the unrestricted graph. It is the
// difference between delivering part of a payment and abandoning it.
func (r *router) salvage(remaining lnwire.MilliSatoshi, busy map[uint64]bool,
	split bool) *plan {

	if !split {
		// Without splitting, only a full-amount attempt helps.
		path, err := r.findPath(remaining, nil)
		if err != nil {
			return nil
		}
		if r.failedSigs[planSig(path, remaining)] {
			return nil
		}
		rt, p, err := r.makeRoute(path, remaining)
		if err != nil {
			return nil
		}
		return &plan{path: path, rt: rt, amt: remaining, prob: p}
	}

	a := remaining
	if single := r.maxLocalEdge(); single >= minShard && single < a {
		a = single
	}
	for i := 0; i < 22 && a >= minShard; i++ {
		for pass := 0; pass < 2; pass++ {
			var avoid map[uint64]bool
			if pass == 0 {
				if len(busy) == 0 {
					continue
				}
				avoid = busy
			}
			path, err := r.findPath(a, avoid)
			if err != nil {
				continue
			}
			if pl := r.bestOnPath(path, a, remaining); pl != nil {
				return pl
			}
		}
		a = a * 2 / 3
	}
	return nil
}

// hopAmount returns the amount flowing over hop index i of a route.
func hopAmount(rt *route.Route, i int) lnwire.MilliSatoshi {
	if i == 0 {
		return rt.TotalAmount
	}
	return rt.Hops[i-1].AmtToForward
}

func hopKey(h *route.Hop) edgeKey {
	return edgeKey{chanID: h.ChannelID, to: h.PubKeyBytes}
}

// markInFlight adjusts the in-flight accounting for a route.
func (r *router) markInFlight(rt *route.Route, sign int) {
	for i, h := range rt.Hops {
		b := r.bel(hopKey(h))
		a := hopAmount(rt, i)
		if sign > 0 {
			b.inFlight += a
		} else if b.inFlight >= a {
			b.inFlight -= a
		} else {
			b.inFlight = 0
		}
	}
}

// noteRoute records the route as pending and reserves its liquidity.
func (r *router) noteRoute(attemptID uint64, rt *route.Route) {
	if _, ok := r.pending[attemptID]; ok {
		return
	}
	r.pending[attemptID] = rt
	r.markInFlight(rt, +1)
}

// provePassed raises the lower bound on a hop that demonstrably forwarded
// but whose HTLC did not settle, so no liquidity actually moved.
func (r *router) provePassed(h *route.Hop, a lnwire.MilliSatoshi) {
	b := r.bel(hopKey(h))
	if a > b.okAmt {
		b.okAmt = a
	}
	b.succ = true

	// A success invalidates any older failure bound at or below this
	// amount, and clears accumulated suspicion.
	if b.hasFail && b.failAmt <= a {
		b.hasFail = false
		b.failAmt = 0
		b.fails = 0
	}
	b.misses = 0
}

// applySettle folds a settled forward into both directions of the channel:
// the sending direction lost exactly that much liquidity and the receiving
// direction gained it.
func (r *router) applySettle(from route.Vertex, h *route.Hop,
	a lnwire.MilliSatoshi) {

	k := hopKey(h)
	b := r.bel(k)
	b.succ = true
	if b.okAmt < a {
		b.okAmt = a
	}
	b.okAmt -= a
	b.drained += a
	b.fails = 0
	if b.hasFail {
		if b.failAmt > a {
			b.failAmt -= a
		} else {
			b.failAmt = 1
		}
	}
	b.misses = 0

	// The reverse direction gained exactly what we pushed.
	rk := edgeKey{chanID: k.chanID, to: from}
	rb := r.bel(rk)
	rb.okAmt += a
	rb.misses = 0
	if rb.drained > a {
		rb.drained -= a
	} else {
		rb.drained = 0
	}
	if rb.hasFail {
		rb.failAmt += a
		if c := r.capOf(rk); c > 0 && rb.failAmt >= c {
			rb.hasFail = false
			rb.failAmt = 0
			rb.fails = 0
		}
		if rb.hasFail && rb.okAmt >= rb.failAmt {
			rb.hasFail = false
			rb.failAmt = 0
			rb.fails = 0
		}
	}
}

// classify buckets a failure into the kind of repair it implies. The string
// form is used so this works whatever concrete shape the failure takes.
func classify(f any) string {
	d := fmt.Sprintf("%T %v", f, f)
	switch {
	case strings.Contains(d, "FeeInsufficient"):
		return "fee"
	case strings.Contains(d, "CltvExpiry"),
		strings.Contains(d, "ExpiryTooSoon"):
		return "cltv"
	case strings.Contains(d, "AmountBelowMinimum"):
		return "min"
	case strings.Contains(d, "ChannelDisabled"),
		strings.Contains(d, "UnknownNextPeer"),
		strings.Contains(d, "PermanentChannelFailure"):
		return "dead"
	}
	return "liquidity"
}

// ReportAttempt folds attempt feedback into the liquidity beliefs.
//
// NOTE: Part of the routing.SimRouter interface.
func (r *router) ReportAttempt(attemptID uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if rt == nil {
		return nil
	}

	// Release any liquidity we had reserved for this attempt.
	if prev, ok := r.pending[attemptID]; ok {
		r.markInFlight(prev, -1)
		delete(r.pending, attemptID)
	}

	// Success: every hop carried its amount and the liquidity moved, so
	// shift both directions of every channel on the route.
	if result.Failure == nil {
		r.failStreak = 0
		if n := len(rt.Hops); n > 0 {
			r.delivered += rt.Hops[n-1].AmtToForward
		}
		prev := rt.SourcePubKey
		for i, h := range rt.Hops {
			a := hopAmount(rt, i)
			r.applySettle(prev, h, a)
			prev = h.PubKeyBytes

			if i == 0 {
				if bal, ok := r.localBalances[h.ChannelID]; ok {
					if bal >= a {
						r.localBalances[h.ChannelID] =
							bal - a
					} else {
						r.localBalances[h.ChannelID] = 0
					}
				}
			}
		}
		return nil
	}

	if sig := routeSig(rt); sig != "" {
		r.failedSigs[sig] = true
	}

	// Locate the failing hop: the failure source is the node that could
	// not forward, so the offending channel is its outgoing hop.
	failIdx := -1
	if result.FailureSource == rt.SourcePubKey {
		failIdx = 0
	} else {
		for i, h := range rt.Hops {
			if h.PubKeyBytes == result.FailureSource {
				failIdx = i + 1
				break
			}
		}
	}

	// A failure deep in the route means the earlier hops demonstrably
	// forwarded, which is real progress: we learned usable bounds. Charge
	// such a failure at half weight so a large multi-shard payment is not
	// killed by informative misses.
	if failIdx > 1 {
		r.failStreak++
		if r.failStreak > 0 && r.attempts%2 == 0 {
			r.failStreak--
		}
	} else {
		r.failStreak++
	}

	// An unattributable failure: spread suspicion over the remote hops so
	// we stop re-picking this corridor without destroying the hard bounds
	// we have earned.
	if failIdx < 0 || failIdx >= len(rt.Hops) {
		for i, h := range rt.Hops {
			if i == 0 {
				continue
			}
			b := r.bel(hopKey(h))
			if b.misses < 3 {
				b.misses++
			}
			// Optimism earned from older evidence cannot survive a
			// corridor that just failed somewhere inside it.
			b.succ = false
			r.pruneQueue(h.ChannelID)
		}
		return nil
	}

	// Everything strictly before the failing hop demonstrably worked, but
	// nothing settled, so no liquidity actually moved.
	for i := 0; i < failIdx; i++ {
		r.provePassed(rt.Hops[i], hopAmount(rt, i))
	}

	h := rt.Hops[failIdx]
	a := hopAmount(rt, failIdx)
	b := r.bel(hopKey(h))
	e := r.byKey[hopKey(h)]

	switch classify(result.Failure) {
	case "fee":
		// Our fee view for this hop is stale: pay more next time. A
		// generous bump is cheaper than another failed attempt. A fee
		// repair is not a liquidity miss, so it does not count against
		// the streak and the queued plan stays valid.
		if e != nil {
			e.baseFee += e.baseFee/4 + 1_000
			e.feePPM += e.feePPM/4 + 50
		}
		if r.failStreak > 0 {
			r.failStreak--
		}
		return nil

	case "cltv":
		if e != nil {
			e.cltv += e.cltv/4 + 20
		}
		if r.failStreak > 0 {
			r.failStreak--
		}
		return nil

	case "min":
		// The advertised minimum is higher than we thought.
		if e != nil && a >= e.minHTLC {
			e.minHTLC = a + 1
		}
		if r.failStreak > 0 {
			r.failStreak--
		}
		return nil

	case "dead":
		b.dead = true
		r.pruneQueue(h.ChannelID)
		return nil
	}

	// Liquidity miss: tighten the upper bound on this direction.
	if !b.hasFail || a < b.failAmt {
		b.hasFail = true
		b.failAmt = a
	}
	b.fails++
	// The failure bound must sit above the proven-good bound.
	if b.okAmt >= b.failAmt {
		if b.failAmt > 0 {
			b.okAmt = b.failAmt - 1
		} else {
			b.okAmt = 0
		}
	}
	// A hard miss overrides bimodal optimism from older evidence.
	b.succ = false
	b.drained = 0

	// Only the shards routed through the failing channel are invalidated:
	// the rest of a joint plan is still the best decomposition we have, and
	// throwing it away is what previously collapsed large payments back
	// into single-corridor grinding.
	r.pruneQueue(h.ChannelID)

	// A local channel failing means our balance estimate was too high, and
	// it also invalidates the complementary inference that the far side of
	// that channel is the empty one.
	if failIdx == 0 {
		rk := edgeKey{chanID: h.ChannelID, to: rt.SourcePubKey}
		if rb, ok := r.beliefs[rk]; ok {
			rb.hasFail = false
			rb.failAmt = 0
		}
		if bal, ok := r.localBalances[h.ChannelID]; ok && bal >= a {
			if a == 0 {
				r.localBalances[h.ChannelID] = 0
			} else {
				r.localBalances[h.ChannelID] = a - 1
			}
		}
	}

	return nil
}
