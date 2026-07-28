package main

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

const finalCltvDelta = 40

type candidateEdgeKey struct {
	chanID   uint64
	from, to route.Vertex
}

type candidateEdge struct {
	key      candidateEdgeKey
	capacity lnwire.MilliSatoshi

	baseFeeMsat   lnwire.MilliSatoshi
	feeRatePPM    lnwire.MilliSatoshi
	timeLockDelta uint16
	minHTLC       lnwire.MilliSatoshi
	maxHTLC       lnwire.MilliSatoshi
}

func (e *candidateEdge) fee(
	amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return e.baseFeeMsat + amt*e.feeRatePPM/1_000_000
}

func (e *candidateEdge) policyAllows(
	amt lnwire.MilliSatoshi) bool {

	if amt < e.minHTLC || amt > e.capacity {
		return false
	}

	return e.maxHTLC == 0 || amt <= e.maxHTLC
}

type candidateBelief struct {
	lowerOK   lnwire.MilliSatoshi
	upperFail lnwire.MilliSatoshi
	estimate  lnwire.MilliSatoshi
	conf      uint8

	suspectAmt    lnwire.MilliSatoshi
	suspectWeight float64
}

var candidateBeliefStore = struct {
	sync.Mutex
	beliefs map[candidateEdgeKey]*candidateBelief
}{
	beliefs: make(map[candidateEdgeKey]*candidateBelief),
}

type candidateLocalFailure struct {
	upper  lnwire.MilliSatoshi
	weight float64
}

type candidateTraversal struct {
	key  candidateEdgeKey
	edge *candidateEdge
	amt  lnwire.MilliSatoshi
}

type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edges         map[candidateEdgeKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	reserved    map[candidateEdgeKey]lnwire.MilliSatoshi
	usedTotals  map[candidateEdgeKey]lnwire.MilliSatoshi
	localFails  map[candidateEdgeKey]candidateLocalFailure
	edgePenalty map[candidateEdgeKey]float64

	plannedParts   uint32
	failures       uint32
	unknownFails   uint32
	successfulParts uint32
	retryCap       lnwire.MilliSatoshi
	delivered      lnwire.MilliSatoshi
	settled        bool
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	r := &candidateRouter{
		source:        source,
		spec:          spec,
		incomingEdges: make(map[route.Vertex][]*candidateEdge),
		edges:         make(map[candidateEdgeKey]*candidateEdge),
		localBalances: localBalances,
		reserved:      make(map[candidateEdgeKey]lnwire.MilliSatoshi),
		usedTotals:    make(map[candidateEdgeKey]lnwire.MilliSatoshi),
		localFails:    make(map[candidateEdgeKey]candidateLocalFailure),
		edgePenalty:   make(map[candidateEdgeKey]float64),
	}

	ctx := context.Background()
	seen := map[route.Vertex]bool{source: true}
	queue := []route.Vertex{source}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		err := view.ForEachNodeDirectedChannel(
			ctx, node,
			func(ch *graphdb.DirectedChannel) error {
				if !seen[ch.OtherNode] {
					seen[ch.OtherNode] = true
					queue = append(queue, ch.OtherNode)
				}

				policy := ch.InPolicy
				if policy == nil || policy.IsDisabled {
					return nil
				}

				key := candidateEdgeKey{
					chanID: ch.ChannelID,
					from:   ch.OtherNode,
					to:     node,
				}
				edge := &candidateEdge{
					key: key,
					capacity: lnwire.NewMSatFromSatoshis(
						ch.Capacity,
					),
					baseFeeMsat:   policy.FeeBaseMSat,
					feeRatePPM:    policy.FeeProportionalMillionths,
					timeLockDelta: policy.TimeLockDelta,
					minHTLC:       policy.MinHTLC,
				}
				if policy.HasMaxHTLC {
					edge.maxHTLC = policy.MaxHTLC
				}

				r.incomingEdges[node] = append(
					r.incomingEdges[node], edge,
				)
				r.edges[key] = edge

				return nil
			},
			func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	r.plannedParts = r.initialPartCount(spec.Amount)
	if spec.MaxParts != 0 && r.plannedParts > spec.MaxParts {
		r.plannedParts = spec.MaxParts
	}
	if r.plannedParts == 0 {
		r.plannedParts = 1
	}

	return r, nil
}

func (r *candidateRouter) initialPartCount(
	amt lnwire.MilliSatoshi) uint32 {

	switch {
	case amt <= 20_000_000:
		return 1

	case amt <= 75_000_000:
		return 3

	case amt <= 200_000_000:
		return 5

	case amt <= 400_000_000:
		return 8

	case amt <= 900_000_000:
		return 10

	case amt <= 2_000_000_000:
		return 12

	default:
		return 16
	}
}

func candidatePrior(amt,
	capacity lnwire.MilliSatoshi) float64 {

	if capacity <= 0 || amt > capacity {
		return 0.003
	}

	x := float64(amt) / float64(capacity)

	lowMode := 0.48 * math.Exp(-x/0.025)
	highMode := 0.50 / (1 + math.Exp((x-0.90)/0.025))
	p := 0.005 + lowMode + highMode

	switch {
	case p < 0.005:
		return 0.005

	case p > 0.985:
		return 0.985

	default:
		return p
	}
}

func candidateLogisticProbability(amt, estimate,
	capacity lnwire.MilliSatoshi) float64 {

	if capacity <= 0 {
		return 0.005
	}

	scale := 0.065 * float64(capacity)
	if scale < 1 {
		scale = 1
	}

	z := (float64(amt) - float64(estimate)) / scale
	switch {
	case z > 30:
		return 0.005

	case z < -30:
		return 0.995

	default:
		return 1 / (1 + math.Exp(z))
	}
}

func candidateClampProbability(p float64) float64 {
	switch {
	case p < 0.003:
		return 0.003

	case p > 0.995:
		return 0.995

	default:
		return p
	}
}

func (r *candidateRouter) edgeProbability(e *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	required := amt + r.reserved[e.key]
	if required > e.capacity {
		return 0.001
	}

	if e.key.from == r.source {
		if r.localBalances[e.key.chanID] < required {
			return 0.001
		}

		return 0.999
	}

	p := candidatePrior(required, e.capacity)

	candidateBeliefStore.Lock()
	stored := candidateBeliefStore.beliefs[e.key]
	var belief candidateBelief
	if stored != nil {
		belief = *stored
	}
	candidateBeliefStore.Unlock()

	if stored != nil {
		if belief.estimate > 0 && belief.conf > 0 {
			estimateP := candidateLogisticProbability(
				required, belief.estimate, e.capacity,
			)

			weight := 0.13 * float64(belief.conf)
			if weight > 0.78 {
				weight = 0.78
			}
			p = (1-weight)*p + weight*estimateP
		}

		if belief.lowerOK > 0 && required <= belief.lowerOK {
			p = math.Max(p, 0.995)
		}

		if belief.upperFail > 0 {
			switch {
			case required >= belief.upperFail:
				p = math.Min(p, 0.012)

			case belief.lowerOK > 0 &&
				belief.upperFail > belief.lowerOK &&
				required > belief.lowerOK:

				span := float64(
					belief.upperFail - belief.lowerOK,
				)
				position := float64(
					required - belief.lowerOK,
				) / span

				bounded := 0.995*(1-position) +
					0.012*position
				p = 0.30*p + 0.70*bounded
			}
		}
	}

	if local, ok := r.localFails[e.key]; ok &&
		local.upper > 0 && required >= local.upper {

		p *= math.Exp(-1.85 * local.weight)
	}

	return candidateClampProbability(p)
}

func (r *candidateRouter) edgeCost(e *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	p := r.edgeProbability(e, amt)
	cost := -math.Log(p) + 0.018 + r.edgePenalty[e.key]

	if reserved := r.reserved[e.key]; reserved > 0 &&
		e.capacity > 0 {

		ratio := float64(reserved) / float64(e.capacity)

		// A successful shard is evidence that the corridor is live, so
		// reuse remains possible. The increasing marginal charge keeps
		// atomic siblings from all leaning on a narrow channel.
		cost += 0.08 + 0.65*ratio*ratio
	}

	return cost
}

type candidateDijkstraItem struct {
	node  route.Vertex
	score float64
	amt   lnwire.MilliSatoshi
}

type candidateDijkstraQueue []*candidateDijkstraItem

func (q candidateDijkstraQueue) Len() int {
	return len(q)
}

func (q candidateDijkstraQueue) Less(i, j int) bool {
	return q[i].score < q[j].score
}

func (q candidateDijkstraQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

func (q *candidateDijkstraQueue) Push(x any) {
	*q = append(*q, x.(*candidateDijkstraItem))
}

func (q *candidateDijkstraQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[:n-1]

	return item
}

func (r *candidateRouter) findRoute(
	amt lnwire.MilliSatoshi) (*route.Route, float64, error) {

	if amt <= 0 {
		return nil, 0, errors.New("invalid route amount")
	}

	score := make(map[route.Vertex]float64)
	arriving := make(map[route.Vertex]lnwire.MilliSatoshi)
	next := make(map[route.Vertex]*candidateEdge)

	score[r.spec.Target] = 0
	arriving[r.spec.Target] = amt

	pq := &candidateDijkstraQueue{}
	heap.Push(pq, &candidateDijkstraItem{
		node: r.spec.Target,
		amt:  amt,
	})

	for pq.Len() > 0 {
		item := heap.Pop(pq).(*candidateDijkstraItem)

		bestScore, ok := score[item.node]
		if !ok || item.score > bestScore+1e-12 {
			continue
		}
		if arriving[item.node] != item.amt {
			continue
		}
		if item.node == r.source {
			break
		}

		for _, edge := range r.incomingEdges[item.node] {
			amtOverEdge := item.amt
			if !edge.policyAllows(amtOverEdge) {
				continue
			}

			required := amtOverEdge + r.reserved[edge.key]
			if required > edge.capacity {
				continue
			}
			if edge.key.from == r.source &&
				r.localBalances[edge.key.chanID] < required {

				continue
			}

			sending := amtOverEdge
			feeCost := 0.0
			if edge.key.from != r.source {
				fee := edge.fee(amtOverEdge)
				sending += fee

				denominator := float64(amt)
				if denominator < 1 {
					denominator = 1
				}
				feeCost = 42 * float64(fee) / denominator
			}

			newScore := item.score +
				r.edgeCost(edge, amtOverEdge) + feeCost

			oldScore, exists := score[edge.key.from]
			if exists && newScore >= oldScore {
				continue
			}

			score[edge.key.from] = newScore
			arriving[edge.key.from] = sending
			next[edge.key.from] = edge

			heap.Push(pq, &candidateDijkstraItem{
				node:  edge.key.from,
				score: newScore,
				amt:   sending,
			})
		}
	}

	if _, ok := score[r.source]; !ok {
		return nil, 0, errors.New("no route found")
	}

	rt, err := r.buildRoute(amt, next)
	if err != nil {
		return nil, 0, err
	}

	probability := 1.0
	for _, traversal := range r.routeTraversals(rt) {
		probability *= r.edgeProbability(
			traversal.edge, traversal.amt,
		)
	}

	return rt, probability, nil
}

func (r *candidateRouter) buildRoute(amt lnwire.MilliSatoshi,
	next map[route.Vertex]*candidateEdge) (*route.Route, error) {

	var path []*candidateEdge
	for node := r.source; node != r.spec.Target; {
		edge, ok := next[node]
		if !ok {
			return nil, fmt.Errorf("broken path at %v", node)
		}

		path = append(path, edge)
		node = edge.key.to
	}

	if len(path) == 0 {
		return nil, errors.New("empty route")
	}

	amounts := make([]lnwire.MilliSatoshi, len(path))
	expiries := make([]uint32, len(path))

	last := len(path) - 1
	amounts[last] = amt
	expiries[last] = finalCltvDelta

	for i := last - 1; i >= 0; i-- {
		forwardingEdge := path[i+1]

		amounts[i] = amounts[i+1] +
			forwardingEdge.fee(amounts[i+1])
		expiries[i] = expiries[i+1] +
			uint32(forwardingEdge.timeLockDelta)
	}

	hops := make([]*route.Hop, len(path))
	for i, edge := range path {
		amountToForward := amt
		outgoingExpiry := uint32(finalCltvDelta)

		if i < last {
			amountToForward = amounts[i+1]
			outgoingExpiry = expiries[i+1]
		}

		hops[i] = &route.Hop{
			PubKeyBytes:      edge.key.to,
			ChannelID:        edge.key.chanID,
			AmtToForward:     amountToForward,
			OutgoingTimeLock: outgoingExpiry,
		}
	}

	return &route.Route{
		TotalTimeLock: expiries[0],
		TotalAmount:   amounts[0],
		SourcePubKey:  r.source,
		Hops:          hops,
	}, nil
}

func candidateCeilDiv(amt lnwire.MilliSatoshi,
	parts uint32) lnwire.MilliSatoshi {

	if parts <= 1 {
		return amt
	}

	divisor := lnwire.MilliSatoshi(parts)
	return (amt + divisor - 1) / divisor
}

func (r *candidateRouter) targetPartCount() uint32 {
	target := r.plannedParts

	// Failed large shards make additional slots valuable. Unknown
	// failures diversify routes but do not immediately force a probing
	// ladder because they carry no amount bound.
	target += r.failures / 3
	if r.unknownFails >= 4 {
		target++
	}

	maxParts := r.spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}
	if target > maxParts {
		target = maxParts
	}
	if target == 0 {
		target = 1
	}

	return target
}

func (r *candidateRouter) tryExpandedShard(
	base lnwire.MilliSatoshi, amt lnwire.MilliSatoshi,
	baseRoute *route.Route, baseProbability float64) *route.Route {

	if r.successfulParts == 0 || base >= amt {
		return baseRoute
	}

	bestRoute := baseRoute
	bestAmount := base
	bestProbability := baseProbability

	candidates := []lnwire.MilliSatoshi{
		base * 3 / 2,
		base * 2,
		base * 3,
		amt,
	}

	for _, candidate := range candidates {
		if candidate <= bestAmount || candidate > amt {
			continue
		}

		rt, probability, err := r.findRoute(candidate)
		if err != nil {
			continue
		}

		// Scaling is reserved for corridors supported by truthful recent
		// success. This creates unequal MPP allocations without probing
		// an unproven large shard.
		requiredProbability := 0.74
		if len(rt.Hops) >= 12 {
			requiredProbability = 0.68
		}
		if probability < requiredProbability {
			continue
		}
		if probability+0.08 < bestProbability {
			continue
		}

		bestRoute = rt
		bestAmount = candidate
		bestProbability = probability
	}

	return bestRoute
}

func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt <= 0 {
		return nil, errors.New("payment amount is zero")
	}

	maxParts := r.spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}
	if inFlightHtlcs >= maxParts {
		return nil, errors.New("maximum parts already in flight")
	}

	partsLeft := maxParts - inFlightHtlcs
	minimumShard := candidateCeilDiv(amt, partsLeft)

	var wholeRoute *route.Route
	if rt, probability, err := r.findRoute(amt); err == nil {
		wholeRoute = rt

		if r.plannedParts == 1 || probability >= 0.82 {
			return rt, nil
		}
	}

	targetParts := r.targetPartCount()
	desiredSlots := uint32(1)
	if targetParts > inFlightHtlcs {
		desiredSlots = targetParts - inFlightHtlcs
	}
	if desiredSlots > partsLeft {
		desiredSlots = partsLeft
	}

	shard := candidateCeilDiv(amt, desiredSlots)
	if r.retryCap > 0 && r.retryCap < shard {
		shard = r.retryCap
	}
	if shard < minimumShard {
		shard = minimumShard
	}
	if shard > amt {
		shard = amt
	}

	for {
		rt, probability, err := r.findRoute(shard)
		if err == nil {
			return r.tryExpandedShard(
				shard, amt, rt, probability,
			), nil
		}

		if shard <= minimumShard {
			if wholeRoute != nil && shard != amt {
				return wholeRoute, nil
			}

			return nil, err
		}

		nextShard := shard * 2 / 3
		if nextShard < minimumShard {
			nextShard = minimumShard
		}
		if nextShard == shard {
			nextShard--
		}

		shard = nextShard
	}
}

func (r *candidateRouter) routeTraversals(
	rt *route.Route) []candidateTraversal {

	traversals := make(
		[]candidateTraversal, 0, len(rt.Hops),
	)
	from := rt.SourcePubKey

	for i, hop := range rt.Hops {
		key := candidateEdgeKey{
			chanID: hop.ChannelID,
			from:   from,
			to:     hop.PubKeyBytes,
		}
		edge := r.edges[key]
		if edge == nil {
			from = hop.PubKeyBytes
			continue
		}

		amount := rt.TotalAmount
		if i > 0 {
			amount = rt.Hops[i-1].AmtToForward
		}

		traversals = append(traversals, candidateTraversal{
			key:  key,
			edge: edge,
			amt:  amount,
		})
		from = hop.PubKeyBytes
	}

	return traversals
}

func candidateDeliveredAmount(
	rt *route.Route) lnwire.MilliSatoshi {

	if len(rt.Hops) == 0 {
		return 0
	}

	return rt.Hops[len(rt.Hops)-1].AmtToForward
}

func (r *candidateRouter) recordSuccess(rt *route.Route) {
	traversals := r.routeTraversals(rt)

	candidateBeliefStore.Lock()
	for _, traversal := range traversals {
		required := traversal.amt + r.reserved[traversal.key]

		belief := candidateBeliefStore.beliefs[traversal.key]
		if belief == nil {
			belief = &candidateBelief{}
			candidateBeliefStore.beliefs[traversal.key] = belief
		}

		if required > belief.lowerOK {
			belief.lowerOK = required
		}

		highEstimate := traversal.edge.capacity * 88 / 100
		if required > highEstimate {
			highEstimate = required
		}
		if highEstimate > belief.estimate {
			belief.estimate = highEstimate
		}
		if belief.conf < 8 {
			belief.conf++
		}

		if belief.upperFail > 0 &&
			required >= belief.upperFail {

			belief.upperFail = 0
		}
		if belief.suspectAmt > 0 &&
			required >= belief.suspectAmt {

			belief.suspectAmt = 0
			belief.suspectWeight = 0
		}
	}
	candidateBeliefStore.Unlock()

	for _, traversal := range traversals {
		required := traversal.amt + r.reserved[traversal.key]

		if local, ok := r.localFails[traversal.key]; ok &&
			required >= local.upper {

			delete(r.localFails, traversal.key)
		}

		r.reserved[traversal.key] += traversal.amt
		r.usedTotals[traversal.key] += traversal.amt

		// A successful edge should rapidly recover from route-level
		// diversification penalties.
		r.edgePenalty[traversal.key] *= 0.30
	}

	r.successfulParts++
	r.delivered += candidateDeliveredAmount(rt)

	if r.delivered >= r.spec.Amount && !r.settled {
		r.recordSettlement()
		r.settled = true
	}
}

func candidateSubtractFloor(value,
	delta lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	if value > delta {
		return value - delta
	}

	return 0
}

func (r *candidateRouter) recordSettlement() {
	candidateBeliefStore.Lock()
	defer candidateBeliefStore.Unlock()

	for key, used := range r.usedTotals {
		if used <= 0 {
			continue
		}

		if belief := candidateBeliefStore.beliefs[key]; belief != nil {
			belief.lowerOK = candidateSubtractFloor(
				belief.lowerOK, used,
			)
			belief.estimate = candidateSubtractFloor(
				belief.estimate, used,
			)
			belief.upperFail = candidateSubtractFloor(
				belief.upperFail, used,
			)
			belief.suspectAmt = candidateSubtractFloor(
				belief.suspectAmt, used,
			)
			if belief.suspectAmt == 0 {
				belief.suspectWeight = 0
			}
		}

		reverse := candidateEdgeKey{
			chanID: key.chanID,
			from:   key.to,
			to:     key.from,
		}
		reverseBelief := candidateBeliefStore.beliefs[reverse]
		if reverseBelief == nil {
			reverseBelief = &candidateBelief{}
			candidateBeliefStore.beliefs[reverse] = reverseBelief
		}

		reverseCapacity := lnwire.MilliSatoshi(0)
		if edge := r.edges[reverse]; edge != nil {
			reverseCapacity = edge.capacity
		} else if edge := r.edges[key]; edge != nil {
			reverseCapacity = edge.capacity
		}

		reverseBelief.lowerOK += used
		reverseBelief.estimate += used

		if reverseCapacity > 0 {
			if reverseBelief.lowerOK > reverseCapacity {
				reverseBelief.lowerOK = reverseCapacity
			}
			if reverseBelief.estimate > reverseCapacity {
				reverseBelief.estimate = reverseCapacity
			}
		}

		if reverseBelief.upperFail > 0 {
			reverseBelief.upperFail += used
			if reverseCapacity > 0 &&
				reverseBelief.upperFail > reverseCapacity {

				reverseBelief.upperFail = reverseCapacity
			}
		}
	}
}

func (r *candidateRouter) failureTraversalIndex(
	rt *route.Route, source route.Vertex) (int, bool) {

	if source == rt.SourcePubKey {
		return 0, len(rt.Hops) > 0
	}

	for i, hop := range rt.Hops {
		if hop.PubKeyBytes != source {
			continue
		}

		index := i + 1
		if index >= len(rt.Hops) {
			return 0, false
		}

		return index, true
	}

	return 0, false
}

func (r *candidateRouter) addLocalFailure(
	traversal candidateTraversal, weight float64) {

	required := traversal.amt + r.reserved[traversal.key]

	local := r.localFails[traversal.key]
	if local.upper == 0 || required < local.upper {
		local.upper = required
	}
	local.weight += weight
	if local.weight > 4 {
		local.weight = 4
	}
	r.localFails[traversal.key] = local
}

func (r *candidateRouter) recordPersistentFailure(
	traversal candidateTraversal, weight float64) {

	if traversal.key.from == r.source {
		return
	}

	required := traversal.amt + r.reserved[traversal.key]

	candidateBeliefStore.Lock()
	defer candidateBeliefStore.Unlock()

	belief := candidateBeliefStore.beliefs[traversal.key]
	if belief == nil {
		belief = &candidateBelief{}
		candidateBeliefStore.beliefs[traversal.key] = belief
	}

	if belief.suspectAmt == 0 || required < belief.suspectAmt {
		belief.suspectAmt = required
	}
	belief.suspectWeight += weight

	// Three direct reports are needed before noisy attribution becomes
	// a persistent bound. Until then it affects only this payment.
	if belief.suspectWeight < 2.05 {
		return
	}

	if belief.upperFail == 0 ||
		belief.suspectAmt < belief.upperFail {

		belief.upperFail = belief.suspectAmt
	}

	failedEstimate := belief.suspectAmt * 68 / 100
	if belief.estimate == 0 ||
		failedEstimate < belief.estimate {

		belief.estimate = failedEstimate
	}
	if belief.conf < 8 {
		belief.conf++
	}
}

func (r *candidateRouter) recordAttributedLiquidityFailure(
	traversals []candidateTraversal, claimed int) {

	for offset := -1; offset <= 1; offset++ {
		index := claimed + offset
		if index < 0 || index >= len(traversals) {
			continue
		}

		weight := 0.14
		penalty := 0.20
		if offset == 0 {
			weight = 0.72
			penalty = 1.05
		}

		traversal := traversals[index]
		r.addLocalFailure(traversal, weight)
		r.edgePenalty[traversal.key] += penalty

		// Only the reported edge contributes to persistent evidence.
		// Adjacent weights are useful inside this payment for shifted
		// blame, but are too ambiguous to retain across payments.
		if offset == 0 {
			r.recordPersistentFailure(traversal, weight)
		}
	}
}

func (r *candidateRouter) recordPolicyFailure(
	traversals []candidateTraversal, claimed int) {

	for offset := -1; offset <= 1; offset++ {
		index := claimed + offset
		if index < 0 || index >= len(traversals) {
			continue
		}

		penalty := 0.35
		if offset == 0 {
			penalty = 1.80
		}
		r.edgePenalty[traversals[index].key] += penalty
	}
}

func (r *candidateRouter) ReportAttempt(attemptID uint64,
	rt *route.Route, result routing.SimHtlcResult) error {

	if result.Failure == nil {
		r.retryCap = 0
		r.recordSuccess(rt)
		return nil
	}

	r.failures++

	traversals := r.routeTraversals(rt)
	if len(traversals) != len(rt.Hops) {
		r.unknownFails++
		return nil
	}

	for _, traversal := range traversals {
		r.edgePenalty[traversal.key] += 0.025
	}

	code := result.Failure.Code()
	claimed, attributed := r.failureTraversalIndex(
		rt, result.FailureSource,
	)

	switch code {
	case lnwire.CodeTemporaryChannelFailure:
		if attributed {
			r.recordAttributedLiquidityFailure(
				traversals, claimed,
			)

			delivered := candidateDeliveredAmount(rt)
			if delivered > 1 {
				next := delivered * 58 / 100
				if r.retryCap == 0 || next < r.retryCap {
					r.retryCap = next
				}
			}

			return nil
		}

	case lnwire.CodeFeeInsufficient,
		lnwire.CodeIncorrectCltvExpiry:

		if attributed {
			r.recordPolicyFailure(traversals, claimed)
			return nil
		}
	}

	// Unreadable failures write no liquidity observation. A modest
	// payment-local route penalty encourages a genuinely different
	// attempt without poisoning shared beliefs or forcing tiny probes.
	r.unknownFails++
	routePenalty := 0.16
	if len(traversals) > 12 {
		routePenalty = 0.11
	}
	for _, traversal := range traversals {
		r.edgePenalty[traversal.key] += routePenalty
	}

	return nil
}
