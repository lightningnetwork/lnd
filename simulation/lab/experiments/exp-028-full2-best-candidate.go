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
	minRiskPriceMsat  = float64(60_000)

	hopCostMsat        = float64(85_000)
	reusedEdgeCostMsat = float64(1_600_000)
	extraPartCostMsat  = float64(55_000)

	maxSearchHops        = 24
	maxLabelsPerNode     = 5
	maxRouterAttempts    = 40
	maxConsecutiveMisses = 32
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
	if outbound < 0 {
		outbound = 0
	}

	inboundBasis := int64(amt) + outbound
	inbound := int64(in.inboundBase) +
		inboundBasis*int64(in.inboundRate)/1_000_000

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

	reserved map[edgeKey]lnwire.MilliSatoshi
	held     []*routeMeta

	localUpper  map[edgeKey]lnwire.MilliSatoshi
	localFails  map[edgeKey]int
	blocked     map[edgeKey]bool
	edgePenalty map[edgeKey]float64

	liquiditySuspects map[edgeKey]int
	policySuspects    map[edgeKey]int

	issued map[*route.Route]*routeMeta
	plan   []*plannedRoute

	committedFees lnwire.MilliSatoshi

	attempts          int
	consecutiveMisses int
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	r := &candidateRouter{
		view:              view,
		source:            source,
		spec:              spec,
		incomingEdges:     make(map[route.Vertex][]*candidateEdge),
		edgeLookup:        make(map[edgeKey]*candidateEdge),
		localBalances:     localBalances,
		reserved:          make(map[edgeKey]lnwire.MilliSatoshi),
		localUpper:        make(map[edgeKey]lnwire.MilliSatoshi),
		localFails:        make(map[edgeKey]int),
		blocked:           make(map[edgeKey]bool),
		edgePenalty:       make(map[edgeKey]float64),
		liquiditySuspects: make(map[edgeKey]int),
		policySuspects:    make(map[edgeKey]int),
		issued:            make(map[*route.Route]*routeMeta),
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
	weight float64, now time.Time) {

	if amt <= 0 || weight <= 0 {
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

	b.conf = math.Min(1, b.conf+weight)
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
	kept := make([]*searchState, 0, len(old)+1)

	for _, other := range old {
		if !other.active {
			continue
		}

		if other.score <= s.score &&
			other.fee <= s.fee &&
			other.amount <= s.amount {

			labels[s.node] = append(kept, other)
			for _, tail := range old {
				if tail.active && tail != other {
					labels[s.node] = append(labels[s.node], tail)
				}
			}
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
		protected := 0
		for i := 1; i < len(kept); i++ {
			if kept[i].fee < kept[protected].fee {
				protected = i
			}
		}

		worst := -1
		for i := range kept {
			if i == protected {
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
	if r.committedFees >= r.spec.FeeLimitMsat {
		return 0
	}
	return r.spec.FeeLimitMsat - r.committedFees
}

func riskPrice(feeCap lnwire.MilliSatoshi) float64 {
	if feeCap == lnwire.MaxMilliSatoshi {
		return baseRiskPriceMsat
	}

	price := float64(feeCap) * 0.75
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

	cutoff := upper * 7 / 8
	if r.localFails[key] >= 3 {
		cutoff = upper * 2 / 3
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

			reserved := reservations[edge.key]
			if reserved > lnwire.MaxMilliSatoshi-over {
				continue
			}
			totalLoad := over + reserved
			if totalLoad > edge.capacity {
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

			if nodeCharge > lnwire.MaxMilliSatoshi-cur.fee {
				continue
			}
			totalFee := cur.fee + nodeCharge
			if feeCap != lnwire.MaxMilliSatoshi &&
				totalFee > feeCap {

				continue
			}

			p := channelProbability(
				edge.key, totalLoad, edge.capacity, now,
			)
			if edge.from == r.source {
				p = 0.995
				if r.localUpper[edge.key] > 0 {
					p = 0.82
				}
			}

			score := cur.score +
				float64(nodeCharge) +
				price*(-math.Log(p)) +
				hopCostMsat +
				r.edgePenalty[edge.key]

			if reserved > 0 {
				reuse := reusedEdgeCostMsat
				if edge.from == r.source {
					reuse *= 0.6
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

	var weightSum int64
	for _, weight := range weights {
		if weight > 0 {
			weightSum += weight
		}
	}

	result := make([]lnwire.MilliSatoshi, len(weights))
	if total < lnwire.MilliSatoshi(len(weights)) ||
		len(weights) == 0 || weightSum == 0 {

		return nil
	}

	remaining := total
	remainingWeight := weightSum

	for i, weight := range weights {
		if i == len(weights)-1 {
			result[i] = remaining
			break
		}

		minLater := lnwire.MilliSatoshi(len(weights) - i - 1)
		part := lnwire.MilliSatoshi(
			int64(remaining) * weight / remainingWeight,
		)
		if part <= 0 {
			part = 1
		}
		if part > remaining-minLater {
			part = remaining - minLater
		}

		result[i] = part
		remaining -= part
		remainingWeight -= weight
	}

	return result
}

func allocationPatterns(total lnwire.MilliSatoshi,
	parts int) [][]lnwire.MilliSatoshi {

	if parts <= 0 || total < lnwire.MilliSatoshi(parts) {
		return nil
	}
	if parts == 1 {
		return [][]lnwire.MilliSatoshi{{total}}
	}

	patterns := make([][]lnwire.MilliSatoshi, 0, 5)

	if parts == 2 {
		for _, weights := range [][]int64{
			{1, 1},
			{1, 2},
			{2, 1},
			{1, 3},
			{3, 1},
		} {
			if allocation := weightedAllocation(total, weights);
				allocation != nil {

				patterns = append(patterns, allocation)
			}
		}
		return patterns
	}

	equal := make([]int64, parts)
	ascending := make([]int64, parts)
	descending := make([]int64, parts)
	for i := 0; i < parts; i++ {
		equal[i] = 1
		ascending[i] = int64(i + 1)
		descending[i] = int64(parts - i)
	}

	for _, weights := range [][]int64{
		equal, ascending, descending,
	} {
		if allocation := weightedAllocation(total, weights);
			allocation != nil {

			patterns = append(patterns, allocation)
		}
	}

	return patterns
}

func candidatePartCounts(slots uint32) []int {
	if slots == 0 {
		return nil
	}

	maxParts := int(slots)
	seen := make(map[int]bool)
	result := make([]int, 0, 6)

	add := func(parts int) {
		if parts < 1 || parts > maxParts || seen[parts] {
			return
		}
		seen[parts] = true
		result = append(result, parts)
	}

	add(1)
	add(2)
	add(3)
	add(4)
	if maxParts > 4 {
		add((maxParts + 4) / 2)
		add(maxParts)
	}

	return result
}

func addReservationSet(dst map[edgeKey]lnwire.MilliSatoshi,
	meta *routeMeta) {

	for i, edge := range meta.path {
		dst[edge.key] += meta.amounts[i]
	}
}

func removeReservationSet(dst map[edgeKey]lnwire.MilliSatoshi,
	meta *routeMeta) {

	for i, edge := range meta.path {
		amount := meta.amounts[i]
		if dst[edge.key] <= amount {
			delete(dst, edge.key)
		} else {
			dst[edge.key] -= amount
		}
	}
}

func (r *candidateRouter) syncHeld(inFlight uint32) bool {
	wanted := int(inFlight)
	if wanted >= len(r.held) {
		return false
	}

	drop := len(r.held) - wanted
	for i := 0; i < drop; i++ {
		removeReservationSet(r.reserved, r.held[i])
	}

	r.held = append([]*routeMeta(nil), r.held[drop:]...)
	return true
}

func (r *candidateRouter) tryAllocation(
	allocation []lnwire.MilliSatoshi,
	feeBudget lnwire.MilliSatoshi) ([]*plannedRoute, float64, bool) {

	reservations := cloneReservations(r.reserved)
	remainingBudget := feeBudget
	plan := make([]*plannedRoute, 0, len(allocation))
	totalScore := float64(0)

	for _, shard := range allocation {
		if shard <= 0 {
			return nil, 0, false
		}

		pr, err := r.findRoute(shard, reservations, remainingBudget)
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
	}

	totalScore += float64(len(plan)-1) * extraPartCostMsat
	return plan, totalScore, true
}

func (r *candidateRouter) makePlan(total lnwire.MilliSatoshi,
	slots uint32, allowPartial bool) ([]*plannedRoute, error) {

	if slots == 0 {
		return nil, errors.New("maximum parts exhausted")
	}

	feeBudget := r.remainingFeeBudget()
	var best []*plannedRoute
	bestScore := math.Inf(1)

	for _, parts := range candidatePartCounts(slots) {
		for _, allocation := range allocationPatterns(total, parts) {
			plan, score, ok := r.tryAllocation(allocation, feeBudget)
			if ok && score < bestScore {
				best = plan
				bestScore = score
			}
		}
	}

	if len(best) > 0 {
		return best, nil
	}

	if allowPartial {
		for _, denominator := range []int64{3, 2, 3, 4} {
			var shard lnwire.MilliSatoshi
			if denominator == 3 && shard == 0 {
				shard = total * 2 / 3
			} else {
				shard = total / lnwire.MilliSatoshi(denominator)
			}

			if shard <= 0 || shard >= total {
				continue
			}

			pr, err := r.findRoute(shard, r.reserved, feeBudget)
			if err == nil {
				return []*plannedRoute{pr}, nil
			}
		}
	}

	return nil, errors.New("no route found")
}

func queuedAmount(plan []*plannedRoute) lnwire.MilliSatoshi {
	var total lnwire.MilliSatoshi
	for _, pr := range plan {
		if pr.meta.delivered > lnwire.MaxMilliSatoshi-total {
			return lnwire.MaxMilliSatoshi
		}
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

	if r.syncHeld(inFlightHtlcs) {
		r.plan = nil
	}

	maxParts := r.spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}
	if inFlightHtlcs >= maxParts {
		return nil, errors.New("maximum parts exhausted")
	}

	slots := maxParts - inFlightHtlcs
	if len(r.plan) > 0 && queuedAmount(r.plan) != amt {
		r.plan = nil
	}

	if len(r.plan) == 0 {
		plan, err := r.makePlan(
			amt, slots, inFlightHtlcs == 0,
		)
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
			return i + 1
		}
	}

	return -1
}

func (r *candidateRouter) penalizeUnattributed(meta *routeMeta) {
	for _, edge := range meta.path {
		r.edgePenalty[edge.key] += 110_000
	}
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
			updateBelief(
				edge.key, meta.loads[i], true, 0.70, now,
			)
			r.edgePenalty[edge.key] *= 0.20
			delete(r.liquiditySuspects, edge.key)
			delete(r.policySuspects, edge.key)

			if upper := r.localUpper[edge.key];
				upper > 0 && meta.loads[i] >= upper {

				delete(r.localUpper, edge.key)
				delete(r.localFails, edge.key)
			}
		}

		addReservationSet(r.reserved, meta)
		r.held = append(r.held, meta)
		r.committedFees += meta.fee
		r.consecutiveMisses = 0
		return nil
	}

	r.plan = nil
	r.consecutiveMisses++

	code := result.Failure.Code()
	reportedIdx := r.failureEdgeIndex(rt, result)
	if reportedIdx < 0 {
		r.penalizeUnattributed(meta)
		return nil
	}

	failIdx := reportedIdx
	if failIdx >= len(meta.path) {
		failIdx = len(meta.path) - 1
	}
	if failIdx < 0 || failIdx >= len(meta.path) {
		r.penalizeUnattributed(meta)
		return nil
	}

	safePassed := failIdx - 1
	for i := 0; i < safePassed; i++ {
		updateBelief(
			meta.path[i].key, meta.loads[i], true, 0.28, now,
		)
	}

	edge := meta.path[failIdx]

	if failIdx > 0 {
		r.edgePenalty[meta.path[failIdx-1].key] += 180_000
	}
	if failIdx+1 < len(meta.path) {
		r.edgePenalty[meta.path[failIdx+1].key] += 180_000
	}

	switch code {
	case lnwire.CodeTemporaryChannelFailure:
		r.liquiditySuspects[edge.key]++
		count := r.liquiditySuspects[edge.key]

		r.edgePenalty[edge.key] +=
			1_800_000 + float64(count)*450_000

		if count == 1 {
			updateBelief(
				edge.key, meta.loads[failIdx], false, 0.18, now,
			)
		}
		if count >= 2 {
			updateBelief(
				edge.key, meta.loads[failIdx], false, 0.48, now,
			)
			r.recordLocalFailure(edge.key, meta.loads[failIdx])
		}

	case lnwire.CodeFeeInsufficient,
		lnwire.CodeIncorrectCltvExpiry:

		r.policySuspects[edge.key]++
		count := r.policySuspects[edge.key]
		r.edgePenalty[edge.key] +=
			2_500_000 + float64(count)*1_000_000

		if count >= 2 {
			r.blocked[edge.key] = true
		}

	default:
		r.edgePenalty[edge.key] += 900_000
	}

	return nil
}