package main

import (
	"container/heap"
	"context"
	"errors"
	"math"
	"sync"
	"time"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	finalCltvDelta = uint32(40)

	baseRiskPriceMsat = float64(420_000)
	minRiskPriceMsat  = float64(30_000)

	hopCostMsat        = float64(5_000)
	reusedEdgeCostMsat = float64(1_100_000)
	extraPartCostMsat  = float64(45_000)

	maxSearchHops        = 60
	maxLabelsPerNode     = 8
	maxRouterAttempts    = 72
	maxConsecutiveMisses = 40
)

type edgeKey struct {
	chanID   uint64
	from, to route.Vertex
}

type liquidityBelief struct {
	lowerOK  lnwire.MilliSatoshi
	upperBad lnwire.MilliSatoshi
	conf     float64
	updated  time.Time
}

var sharedBeliefs = struct {
	sync.Mutex
	m map[edgeKey]liquidityBelief
}{
	m: make(map[edgeKey]liquidityBelief),
}

type candidateEdge struct {
	key      edgeKey
	chanID   uint64
	from, to route.Vertex
	capacity lnwire.MilliSatoshi

	baseFeeMsat   lnwire.MilliSatoshi
	feeRatePPM    lnwire.MilliSatoshi
	timeLockDelta uint16
	minHTLC       lnwire.MilliSatoshi
	maxHTLC       lnwire.MilliSatoshi

	inboundBase int32
	inboundRate int32
}

func (e *candidateEdge) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return e.baseFeeMsat + amt*e.feeRatePPM/1_000_000
}

func (e *candidateEdge) usableHTLC(amt lnwire.MilliSatoshi) bool {
	if amt <= 0 || amt < e.minHTLC || amt > e.capacity {
		return false
	}
	return e.maxHTLC == 0 || amt <= e.maxHTLC
}

func nodeFee(in, out *candidateEdge,
	amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	outbound := int64(out.fee(amt))
	inbound := int64(in.inboundBase) +
		int64(amt)*int64(in.inboundRate)/1_000_000

	total := outbound + inbound
	if total <= 0 {
		return 0
	}

	return lnwire.MilliSatoshi(total)
}

type routeMeta struct {
	path      []*candidateEdge
	amounts   []lnwire.MilliSatoshi
	loads     []lnwire.MilliSatoshi
	delivered lnwire.MilliSatoshi
	fee       lnwire.MilliSatoshi
}

type plannedRoute struct {
	rt    *route.Route
	meta  *routeMeta
	score float64
}

type candidateRouter struct {
	view   routing.SimNetworkView
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edgeLookup    map[edgeKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	// reserved is cumulative load contributed by successful shards in this
	// payment. It models both held atomic shards and liquidity consumed by
	// already-settled non-atomic shards.
	reserved map[edgeKey]lnwire.MilliSatoshi

	// localUpper is deliberately payment-local and stricter than the shared
	// belief. A failed load must be retried substantially lower, rather than
	// being sent repeatedly at nearly the same amount.
	localUpper map[edgeKey]lnwire.MilliSatoshi
	localFails map[edgeKey]int
	blocked    map[edgeKey]bool
	edgePenalty map[edgeKey]float64

	issued map[*route.Route]*routeMeta
	plan   []*plannedRoute

	committedFees lnwire.MilliSatoshi
	budgetGuard   lnwire.MilliSatoshi

	attempts          int
	consecutiveMisses int
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	r := &candidateRouter{
		view:           view,
		source:         source,
		spec:           spec,
		incomingEdges:  make(map[route.Vertex][]*candidateEdge),
		edgeLookup:     make(map[edgeKey]*candidateEdge),
		localBalances:  localBalances,
		reserved:       make(map[edgeKey]lnwire.MilliSatoshi),
		localUpper:     make(map[edgeKey]lnwire.MilliSatoshi),
		localFails:     make(map[edgeKey]int),
		blocked:        make(map[edgeKey]bool),
		edgePenalty:    make(map[edgeKey]float64),
		issued:         make(map[*route.Route]*routeMeta),
	}

	ctx := context.Background()
	seen := map[route.Vertex]bool{source: true}
	queue := []route.Vertex{source}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		err := view.ForEachNodeDirectedChannel(
			ctx, node, func(ch *graphdb.DirectedChannel) error {
				if !seen[ch.OtherNode] {
					seen[ch.OtherNode] = true
					queue = append(queue, ch.OtherNode)
				}

				policy := ch.InPolicy
				if policy == nil || policy.IsDisabled {
					return nil
				}

				key := edgeKey{
					chanID: ch.ChannelID,
					from:   ch.OtherNode,
					to:     node,
				}

				edge := &candidateEdge{
					key:           key,
					chanID:        ch.ChannelID,
					from:          ch.OtherNode,
					to:            node,
					capacity:      lnwire.NewMSatFromSatoshis(ch.Capacity),
					baseFeeMsat:   policy.FeeBaseMSat,
					feeRatePPM:    policy.FeeProportionalMillionths,
					timeLockDelta: policy.TimeLockDelta,
					minHTLC:       policy.MinHTLC,
					inboundBase:   ch.InboundFee.BaseFee,
					inboundRate:   ch.InboundFee.FeeRate,
				}
				if policy.HasMaxHTLC {
					edge.maxHTLC = policy.MaxHTLC
				}

				r.incomingEdges[node] = append(
					r.incomingEdges[node], edge,
				)
				r.edgeLookup[key] = edge

				return nil
			}, func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	return r, nil
}

func bimodalPrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity <= 0 || amt > capacity {
		return 0.005
	}

	x := float64(amt) / float64(capacity)
	lowMode := 0.49 * math.Exp(-x/0.025)
	highMode := 0.49 / (1 + math.Exp((x-0.97)/0.025))
	p := 0.005 + lowMode + highMode

	if p < 0.005 {
		return 0.005
	}
	if p > 0.985 {
		return 0.985
	}

	return p
}

func beliefConfidence(b liquidityBelief, now time.Time) float64 {
	if b.updated.IsZero() || now.Before(b.updated) {
		return 0
	}

	age := now.Sub(b.updated)
	if age <= 0 {
		return b.conf
	}

	return b.conf * math.Exp(-age.Seconds()/1800)
}

func channelProbability(key edgeKey, amt, capacity lnwire.MilliSatoshi,
	now time.Time) float64 {

	prior := bimodalPrior(amt, capacity)

	sharedBeliefs.Lock()
	b, ok := sharedBeliefs.m[key]
	sharedBeliefs.Unlock()
	if !ok {
		return prior
	}

	conf := beliefConfidence(b, now)
	if conf < 0.01 {
		return prior
	}

	evidence := prior
	hasEvidence := false

	switch {
	case b.lowerOK > 0 && amt <= b.lowerOK:
		evidence = 0.995
		hasEvidence = true

	case b.upperBad > 0 && amt >= b.upperBad:
		// Shared failures remain soft because another payment or held shard
		// may have caused a transient depletion.
		evidence = 0.012
		hasEvidence = true

	case b.lowerOK > 0 && b.upperBad > b.lowerOK:
		width := float64(b.upperBad - b.lowerOK)
		pos := float64(b.upperBad-amt) / width
		if pos < 0 {
			pos = 0
		}
		if pos > 1 {
			pos = 1
		}

		evidence = 0.012 + 0.983*pos
		hasEvidence = true
	}

	if !hasEvidence {
		return prior
	}

	p := prior*(1-conf) + evidence*conf
	if p < 0.005 {
		return 0.005
	}
	if p > 0.995 {
		return 0.995
	}

	return p
}

func updateBelief(key edgeKey, amt lnwire.MilliSatoshi, success bool,
	now time.Time) {

	if amt <= 0 {
		return
	}

	sharedBeliefs.Lock()
	defer sharedBeliefs.Unlock()

	b := sharedBeliefs.m[key]
	if !b.updated.IsZero() && now.Before(b.updated) {
		b = liquidityBelief{}
	} else {
		b.conf = beliefConfidence(b, now)
	}

	if success {
		if amt > b.lowerOK {
			b.lowerOK = amt
		}
		if b.upperBad != 0 && amt >= b.upperBad {
			b.upperBad = 0
		}
	} else {
		if b.upperBad == 0 || amt < b.upperBad {
			b.upperBad = amt
		}
		if b.lowerOK >= amt {
			b.lowerOK = 0
		}
	}

	b.conf = math.Min(1, b.conf+0.55)
	b.updated = now
	sharedBeliefs.m[key] = b
}

type searchState struct {
	node     route.Vertex
	amount   lnwire.MilliSatoshi
	fee      lnwire.MilliSatoshi
	score    float64
	hops     int
	nextEdge *candidateEdge
	child    *searchState
	active   bool
}

type searchQueue []*searchState

func (q searchQueue) Len() int {
	return len(q)
}

func (q searchQueue) Less(i, j int) bool {
	return q[i].score < q[j].score
}

func (q searchQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

func (q *searchQueue) Push(x any) {
	*q = append(*q, x.(*searchState))
}

func (q *searchQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[:n-1]
	return item
}

func containsNode(s *searchState, node route.Vertex) bool {
	for cur := s; cur != nil; cur = cur.child {
		if cur.node == node {
			return true
		}
	}

	return false
}

func addSearchLabel(labels map[route.Vertex][]*searchState,
	s *searchState) bool {

	old := labels[s.node]
	kept := old[:0]

	for _, other := range old {
		if !other.active {
			continue
		}

		if other.score <= s.score &&
			other.fee <= s.fee &&
			other.amount <= s.amount {

			return false
		}

		if s.score <= other.score &&
			s.fee <= other.fee &&
			s.amount <= other.amount {

			other.active = false
			continue
		}

		kept = append(kept, other)
	}

	s.active = true
	kept = append(kept, s)

	if len(kept) > maxLabelsPerNode {
		minFeeIdx := 0
		for i := 1; i < len(kept); i++ {
			if kept[i].fee < kept[minFeeIdx].fee {
				minFeeIdx = i
			}
		}

		worst := -1
		for i := range kept {
			if i == minFeeIdx {
				continue
			}
			if worst < 0 || kept[i].score > kept[worst].score {
				worst = i
			}
		}

		if worst >= 0 {
			removed := kept[worst]
			removed.active = false
			kept = append(kept[:worst], kept[worst+1:]...)
			if removed == s {
				labels[s.node] = kept
				return false
			}
		}
	}

	labels[s.node] = kept
	return true
}

func cloneReservations(
	src map[edgeKey]lnwire.MilliSatoshi) map[edgeKey]lnwire.MilliSatoshi {

	dst := make(map[edgeKey]lnwire.MilliSatoshi, len(src))
	for key, amt := range src {
		dst[key] = amt
	}

	return dst
}

func (r *candidateRouter) remainingFeeBudget() lnwire.MilliSatoshi {
	if r.spec.FeeLimitMsat == lnwire.MaxMilliSatoshi {
		return lnwire.MaxMilliSatoshi
	}

	spent := r.committedFees + r.budgetGuard
	if spent >= r.spec.FeeLimitMsat {
		return 0
	}

	return r.spec.FeeLimitMsat - spent
}

func riskPrice(feeCap lnwire.MilliSatoshi) float64 {
	if feeCap == lnwire.MaxMilliSatoshi {
		return baseRiskPriceMsat
	}

	price := float64(feeCap) / 2
	if price < minRiskPriceMsat {
		price = minRiskPriceMsat
	}
	if price > baseRiskPriceMsat {
		price = baseRiskPriceMsat
	}

	return price
}

func (r *candidateRouter) retryCutoff(key edgeKey) lnwire.MilliSatoshi {
	upper := r.localUpper[key]
	if upper <= 0 {
		return 0
	}

	cutoff := upper * 3 / 4
	if r.localFails[key] >= 3 {
		cutoff = upper / 2
	}
	if cutoff <= 0 {
		cutoff = 1
	}

	return cutoff
}

func (r *candidateRouter) findRoute(amt lnwire.MilliSatoshi,
	reservations map[edgeKey]lnwire.MilliSatoshi,
	feeCap lnwire.MilliSatoshi) (*plannedRoute, error) {

	if amt <= 0 {
		return nil, errors.New("invalid route amount")
	}
	if r.source == r.spec.Target {
		return nil, errors.New("source is target")
	}
	if feeCap == 0 {
		return nil, errors.New("fee budget exhausted")
	}

	now := r.view.Now()
	price := riskPrice(feeCap)
	labels := make(map[route.Vertex][]*searchState)
	pq := &searchQueue{}

	start := &searchState{
		node:   r.spec.Target,
		amount: amt,
		active: true,
	}
	labels[start.node] = []*searchState{start}
	heap.Push(pq, start)

	var result *searchState

	for pq.Len() > 0 {
		cur := heap.Pop(pq).(*searchState)
		if !cur.active {
			continue
		}
		if cur.node == r.source {
			result = cur
			break
		}
		if cur.hops >= maxSearchHops {
			continue
		}

		for _, edge := range r.incomingEdges[cur.node] {
			if r.blocked[edge.key] || containsNode(cur, edge.from) {
				continue
			}

			over := cur.amount
			nodeCharge := lnwire.MilliSatoshi(0)

			if cur.node != r.spec.Target {
				if cur.nextEdge == nil {
					continue
				}

				nodeCharge = nodeFee(edge, cur.nextEdge, cur.amount)
				if nodeCharge > lnwire.MaxMilliSatoshi-cur.amount {
					continue
				}
				over += nodeCharge
			}

			if !edge.usableHTLC(over) {
				continue
			}

			totalLoad := over + reservations[edge.key]
			if totalLoad < over || totalLoad > edge.capacity {
				continue
			}

			cutoff := r.retryCutoff(edge.key)
			if cutoff > 0 && totalLoad >= cutoff {
				continue
			}

			if edge.from == r.source {
				balance, ok := r.localBalances[edge.chanID]
				if !ok || totalLoad > balance {
					continue
				}
			}

			totalFee := cur.fee + nodeCharge
			if totalFee < cur.fee {
				continue
			}
			if feeCap != lnwire.MaxMilliSatoshi &&
				totalFee > feeCap {

				continue
			}

			p := channelProbability(
				edge.key, totalLoad, edge.capacity, now,
			)

			if edge.from == r.source {
				// The snapshot makes the first hop nearly certain until an
				// observed local or concurrent reservation failure says
				// otherwise.
				p = 0.995
				if r.localUpper[edge.key] > 0 {
					p = 0.80
				}
			}

			score := cur.score +
				float64(nodeCharge) +
				price*(-math.Log(p)) +
				hopCostMsat +
				r.edgePenalty[edge.key]

			if reservations[edge.key] > 0 {
				reuse := reusedEdgeCostMsat
				if edge.from == r.source {
					reuse *= 0.55
				}
				score += reuse
			}

			next := &searchState{
				node:     edge.from,
				amount:   over,
				fee:      totalFee,
				score:    score,
				hops:     cur.hops + 1,
				nextEdge: edge,
				child:    cur,
			}
			if addSearchLabel(labels, next) {
				heap.Push(pq, next)
			}
		}
	}

	if result == nil {
		return nil, errors.New("no route found")
	}

	path := make([]*candidateEdge, 0, result.hops)
	for cur := result; cur.node != r.spec.Target; cur = cur.child {
		if cur.nextEdge == nil || cur.child == nil {
			return nil, errors.New("broken route state")
		}
		path = append(path, cur.nextEdge)
	}

	rt, amounts, err := r.buildRoute(amt, path)
	if err != nil {
		return nil, err
	}

	loads := make([]lnwire.MilliSatoshi, len(path))
	for i, edge := range path {
		loads[i] = amounts[i] + reservations[edge.key]
	}

	meta := &routeMeta{
		path:      path,
		amounts:   amounts,
		loads:     loads,
		delivered: amt,
		fee:       rt.TotalAmount - amt,
	}

	if feeCap != lnwire.MaxMilliSatoshi && meta.fee > feeCap {
		return nil, errors.New("route exceeds fee budget")
	}

	return &plannedRoute{
		rt:    rt,
		meta:  meta,
		score: result.score,
	}, nil
}

func (r *candidateRouter) buildRoute(amt lnwire.MilliSatoshi,
	path []*candidateEdge) (*route.Route, []lnwire.MilliSatoshi, error) {

	if len(path) == 0 {
		return nil, nil, errors.New("empty path")
	}

	n := len(path)
	amounts := make([]lnwire.MilliSatoshi, n)
	expiries := make([]uint32, n)

	amounts[n-1] = amt
	expiries[n-1] = finalCltvDelta

	for i := n - 2; i >= 0; i-- {
		charge := nodeFee(path[i], path[i+1], amounts[i+1])
		if charge > lnwire.MaxMilliSatoshi-amounts[i+1] {
			return nil, nil, errors.New("route amount overflow")
		}

		amounts[i] = amounts[i+1] + charge
		expiries[i] = expiries[i+1] +
			uint32(path[i+1].timeLockDelta)
	}

	hops := make([]*route.Hop, n)
	for i, edge := range path {
		forward := amt
		expiry := finalCltvDelta

		if i < n-1 {
			forward = amounts[i+1]
			expiry = expiries[i+1]
		}

		hops[i] = &route.Hop{
			PubKeyBytes:      edge.to,
			ChannelID:        edge.chanID,
			AmtToForward:     forward,
			OutgoingTimeLock: expiry,
		}
	}

	return &route.Route{
		TotalTimeLock: expiries[0],
		TotalAmount:   amounts[0],
		SourcePubKey:  r.source,
		Hops:          hops,
	}, amounts, nil
}

func weightedAllocation(total lnwire.MilliSatoshi,
	weights []int64) []lnwire.MilliSatoshi {

	var sum int64
	for _, weight := range weights {
		sum += weight
	}

	result := make([]lnwire.MilliSatoshi, len(weights))
	remaining := total

	for i, weight := range weights {
		if i == len(weights)-1 {
			result[i] = remaining
			break
		}

		part := lnwire.MilliSatoshi(
			int64(total) * weight / sum,
		)
		if part <= 0 {
			part = 1
		}
		if part >= remaining {
			part = remaining - 1
		}
		if part <= 0 {
			part = 1
		}

		result[i] = part
		remaining -= part
	}

	return result
}

func allocationPatterns(total lnwire.MilliSatoshi,
	parts int) [][]lnwire.MilliSatoshi {

	equal := make([]int64, parts)
	for i := range equal {
		equal[i] = 1
	}

	patterns := [][]lnwire.MilliSatoshi{
		weightedAllocation(total, equal),
	}

	switch parts {
	case 2:
		for _, weights := range [][]int64{
			{40, 60}, {60, 40},
			{25, 75}, {75, 25},
		} {
			patterns = append(
				patterns, weightedAllocation(total, weights),
			)
		}

	case 3:
		for _, weights := range [][]int64{
			{20, 30, 50},
			{50, 30, 20},
		} {
			patterns = append(
				patterns, weightedAllocation(total, weights),
			)
		}

	case 4:
		for _, weights := range [][]int64{
			{15, 20, 25, 40},
			{40, 25, 20, 15},
		} {
			patterns = append(
				patterns, weightedAllocation(total, weights),
			)
		}

	default:
		if parts > 4 {
			ascending := make([]int64, parts)
			descending := make([]int64, parts)

			for i := 0; i < parts; i++ {
				ascending[i] = int64(i + 1)
				descending[i] = int64(parts - i)
			}

			patterns = append(
				patterns,
				weightedAllocation(total, ascending),
				weightedAllocation(total, descending),
			)
		}
	}

	return patterns
}

func addReservationSet(dst map[edgeKey]lnwire.MilliSatoshi,
	meta *routeMeta) {

	for i, edge := range meta.path {
		dst[edge.key] += meta.amounts[i]
	}
}

func (r *candidateRouter) tryAllocation(
	allocation []lnwire.MilliSatoshi,
	feeBudget lnwire.MilliSatoshi) ([]*plannedRoute, float64, bool) {

	reservations := cloneReservations(r.reserved)
	remainingBudget := feeBudget
	remainingAmount := lnwire.MilliSatoshi(0)

	for _, shard := range allocation {
		if shard <= 0 {
			return nil, 0, false
		}
		remainingAmount += shard
	}

	plan := make([]*plannedRoute, 0, len(allocation))
	totalScore := float64(0)

	for _, shard := range allocation {
		shardCap := remainingBudget

		// Reserve a proportional share of a finite budget for later
		// shards. A small allowance avoids rejecting a shard merely due
		// to base-fee rounding.
		if remainingBudget != lnwire.MaxMilliSatoshi &&
			remainingAmount > shard {

			shardCap = remainingBudget * shard / remainingAmount
			allowance := remainingBudget / 20
			if allowance > 25_000 {
				allowance = 25_000
			}
			if shardCap <= remainingBudget-allowance {
				shardCap += allowance
			} else {
				shardCap = remainingBudget
			}
		}

		pr, err := r.findRoute(shard, reservations, shardCap)
		if err != nil {
			return nil, 0, false
		}

		if remainingBudget != lnwire.MaxMilliSatoshi {
			if pr.meta.fee > remainingBudget {
				return nil, 0, false
			}
			remainingBudget -= pr.meta.fee
		}

		plan = append(plan, pr)
		totalScore += pr.score
		addReservationSet(reservations, pr.meta)
		remainingAmount -= shard
	}

	totalScore += float64(len(plan)-1) * extraPartCostMsat

	return plan, totalScore, true
}

func (r *candidateRouter) makePlan(total lnwire.MilliSatoshi,
	slots uint32) ([]*plannedRoute, error) {

	if slots == 0 {
		return nil, errors.New("maximum parts exhausted")
	}

	feeBudget := r.remainingFeeBudget()
	if feeBudget == 0 {
		return nil, errors.New("fee budget exhausted")
	}

	var best []*plannedRoute
	bestScore := math.Inf(1)

	for parts := 1; parts <= int(slots); parts++ {
		for _, allocation := range allocationPatterns(total, parts) {
			plan, score, ok := r.tryAllocation(
				allocation, feeBudget,
			)
			if ok && score < bestScore {
				best = plan
				bestScore = score
			}
		}
	}

	if len(best) > 0 {
		return best, nil
	}

	// When the whole route set cannot be formed, make one useful shard.
	// Each failure bound forces the next attempt onto another corridor or
	// onto a materially smaller amount.
	if slots > 1 {
		shard := (total + lnwire.MilliSatoshi(slots) - 1) /
			lnwire.MilliSatoshi(slots)

		for shard > 0 {
			cap := feeBudget
			if feeBudget != lnwire.MaxMilliSatoshi {
				cap = feeBudget * shard / total
				if cap < 1_000 {
					cap = 1_000
				}
			}

			pr, err := r.findRoute(shard, r.reserved, cap)
			if err == nil {
				return []*plannedRoute{pr}, nil
			}

			if shard <= 10_000 {
				break
			}

			next := shard * 2 / 3
			if next >= shard {
				next = shard - 1
			}
			shard = next
		}
	}

	return nil, errors.New("no route found")
}

func queuedAmount(plan []*plannedRoute) lnwire.MilliSatoshi {
	var total lnwire.MilliSatoshi
	for _, pr := range plan {
		total += pr.meta.delivered
	}

	return total
}

func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt <= 0 {
		return nil, errors.New("invalid remaining amount")
	}
	if r.attempts >= maxRouterAttempts {
		return nil, errors.New("attempt limit reached")
	}
	if r.consecutiveMisses >= maxConsecutiveMisses {
		return nil, errors.New("no progress after repeated failures")
	}

	maxParts := r.spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}

	// MaxParts limits concurrent shards, not the lifetime number of
	// successful shards. Atomic held shards appear in inFlightHtlcs,
	// while settled non-atomic shards release their slots.
	if inFlightHtlcs >= maxParts {
		return nil, errors.New("maximum parts exhausted")
	}
	slots := maxParts - inFlightHtlcs

	if len(r.plan) > 0 && queuedAmount(r.plan) != amt {
		r.plan = nil
	}

	if len(r.plan) == 0 {
		plan, err := r.makePlan(amt, slots)
		if err != nil {
			return nil, err
		}
		r.plan = plan
	}

	next := r.plan[0]
	r.plan = r.plan[1:]
	r.issued[next.rt] = next.meta
	r.attempts++

	return next.rt, nil
}

func (r *candidateRouter) metaForRoute(rt *route.Route) *routeMeta {
	if meta, ok := r.issued[rt]; ok {
		return meta
	}
	if rt == nil || len(rt.Hops) == 0 {
		return nil
	}

	path := make([]*candidateEdge, 0, len(rt.Hops))
	amounts := make([]lnwire.MilliSatoshi, 0, len(rt.Hops))
	loads := make([]lnwire.MilliSatoshi, 0, len(rt.Hops))
	from := rt.SourcePubKey

	for i, hop := range rt.Hops {
		key := edgeKey{
			chanID: hop.ChannelID,
			from:   from,
			to:     hop.PubKeyBytes,
		}
		edge := r.edgeLookup[key]
		if edge == nil {
			return nil
		}

		amount := rt.TotalAmount
		if i > 0 {
			amount = rt.Hops[i-1].AmtToForward
		}

		path = append(path, edge)
		amounts = append(amounts, amount)
		loads = append(loads, amount+r.reserved[key])
		from = hop.PubKeyBytes
	}

	delivered := rt.Hops[len(rt.Hops)-1].AmtToForward

	return &routeMeta{
		path:      path,
		amounts:   amounts,
		loads:     loads,
		delivered: delivered,
		fee:       rt.TotalAmount - delivered,
	}
}

func (r *candidateRouter) recordLocalFailure(key edgeKey,
	load lnwire.MilliSatoshi) {

	if load <= 0 {
		return
	}

	upper := r.localUpper[key]
	if upper == 0 || load < upper {
		r.localUpper[key] = load
	}
	r.localFails[key]++
}

func (r *candidateRouter) failureEdgeIndex(rt *route.Route,
	result routing.SimHtlcResult) int {

	if rt == nil {
		return -1
	}
	if result.FailureSource == rt.SourcePubKey {
		return 0
	}

	for i, hop := range rt.Hops {
		if hop.PubKeyBytes == result.FailureSource {
			// A forwarding node reports failure of its outgoing edge.
			return i + 1
		}
	}

	return -1
}

func (r *candidateRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	_ = attemptID

	meta := r.metaForRoute(rt)
	delete(r.issued, rt)
	if meta == nil {
		r.plan = nil
		return nil
	}

	now := r.view.Now()

	if result.Failure == nil {
		for i, edge := range meta.path {
			updateBelief(edge.key, meta.loads[i], true, now)
			r.reserved[edge.key] += meta.amounts[i]
			r.edgePenalty[edge.key] *= 0.30

			if upper := r.localUpper[edge.key];
				upper > 0 && meta.loads[i] >= upper {

				delete(r.localUpper, edge.key)
				delete(r.localFails, edge.key)
			}
		}

		r.committedFees += meta.fee
		r.consecutiveMisses = 0
		return nil
	}

	r.plan = nil
	r.consecutiveMisses++

	code := result.Failure.Code()
	failIdx := r.failureEdgeIndex(rt, result)

	// A source-side liquidity failure may be represented without a public
	// failure source. It is safe and useful to attribute that case to the
	// first channel; treating it as an uninformative dispatch refusal causes
	// an otherwise identical route to be retried until the runner times out.
	if failIdx < 0 &&
		code == lnwire.CodeTemporaryChannelFailure &&
		len(meta.path) > 0 {

		failIdx = 0
	}

	passed := failIdx
	if passed < 0 {
		passed = 0
	}
	if passed > len(meta.path) {
		passed = len(meta.path)
	}

	for i := 0; i < passed; i++ {
		updateBelief(meta.path[i].key, meta.loads[i], true, now)
	}

	if failIdx >= 0 && failIdx < len(meta.path) {
		edge := meta.path[failIdx]

		switch code {
		case lnwire.CodeTemporaryChannelFailure:
			updateBelief(edge.key, meta.loads[failIdx], false, now)
			r.recordLocalFailure(edge.key, meta.loads[failIdx])

			count := r.localFails[edge.key]
			r.edgePenalty[edge.key] +=
				300_000 + float64(count)*175_000

		case lnwire.CodeFeeInsufficient,
			lnwire.CodeIncorrectCltvExpiry:

			// Gossip-policy mismatches are deterministic within this
			// payment and should never poison liquidity estimates.
			r.blocked[edge.key] = true
			r.edgePenalty[edge.key] += 8_000_000

		default:
			r.edgePenalty[edge.key] += 2_000_000
		}

		return nil
	}

	// An unidentified non-liquidity failure provides no trustworthy
	// channel bound. Penalize the whole route to obtain diversity. If a
	// finite budget was involved, retain a small guard against dispatch
	// accounting or rounding differences.
	for _, edge := range meta.path {
		r.edgePenalty[edge.key] += 500_000
	}

	if r.spec.FeeLimitMsat != lnwire.MaxMilliSatoshi {
		guard := meta.fee / 20
		if guard < 1_000 {
			guard = 1_000
		}

		remaining := r.remainingFeeBudget()
		if remaining != lnwire.MaxMilliSatoshi && guard > remaining {
			guard = remaining
		}
		r.budgetGuard += guard
	}

	return nil
}
