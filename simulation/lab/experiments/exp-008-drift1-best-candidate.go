package main

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	candidateFinalCltvDelta = 40
	candidateAttemptLimit   = 48
	candidateMaxRouteHops   = 20
)

type candidateEdgeKey struct {
	chanID uint64
	from   route.Vertex
	to     route.Vertex
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

	const million = lnwire.MilliSatoshi(1_000_000)

	proportional := (amt/million)*e.feeRatePPM +
		(amt%million)*e.feeRatePPM/million

	return e.baseFeeMsat + proportional
}

func (e *candidateEdge) usable(amt lnwire.MilliSatoshi) bool {
	if amt <= 0 || amt < e.minHTLC || amt > e.capacity {
		return false
	}
	if e.maxHTLC != 0 && amt > e.maxHTLC {
		return false
	}

	return true
}

type candidateLiquidityBelief struct {
	capacity  lnwire.MilliSatoshi
	lowerOK   lnwire.MilliSatoshi
	upperFail lnwire.MilliSatoshi
	estimate  lnwire.MilliSatoshi
	conf      float64
	updatedAt time.Time
}

var candidateKnowledge = struct {
	sync.RWMutex
	beliefs map[candidateEdgeKey]candidateLiquidityBelief
}{
	beliefs: make(map[candidateEdgeKey]candidateLiquidityBelief),
}

func candidateReverseKey(key candidateEdgeKey) candidateEdgeKey {
	return candidateEdgeKey{
		chanID: key.chanID,
		from:   key.to,
		to:     key.from,
	}
}

func candidateClampAmount(amt,
	capacity lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	if amt < 0 {
		return 0
	}
	if amt > capacity {
		return capacity
	}

	return amt
}

func candidateNormalizeBelief(
	b candidateLiquidityBelief,
	capacity lnwire.MilliSatoshi) candidateLiquidityBelief {

	b.capacity = capacity
	b.lowerOK = candidateClampAmount(b.lowerOK, capacity)
	b.estimate = candidateClampAmount(b.estimate, capacity)

	if b.upperFail < 0 || b.upperFail > capacity {
		b.upperFail = 0
	}
	if b.upperFail != 0 && b.lowerOK >= b.upperFail {
		b.upperFail = 0
	}
	if b.estimate < b.lowerOK {
		b.estimate = b.lowerOK
	}
	if b.upperFail != 0 && b.estimate >= b.upperFail {
		b.estimate = b.upperFail - 1
		if b.estimate < b.lowerOK {
			b.estimate = b.lowerOK
		}
	}

	if b.conf < 0 {
		b.conf = 0
	}
	if b.conf > 0.99 {
		b.conf = 0.99
	}

	return b
}

func candidateBeliefConfidence(
	b candidateLiquidityBelief, now time.Time) float64 {

	if b.conf <= 0 || b.updatedAt.IsZero() {
		return 0
	}

	age := now.Sub(b.updatedAt).Minutes()
	if age < 0 {
		return 0
	}

	// Background traffic can invalidate old directional evidence quickly.
	const halfLifeMinutes = 35.0
	conf := b.conf * math.Exp(-math.Ln2*age/halfLifeMinutes)

	if conf < 0.01 {
		return 0
	}

	return conf
}

func candidatePrepareObservation(
	b candidateLiquidityBelief, capacity lnwire.MilliSatoshi,
	now time.Time) candidateLiquidityBelief {

	if b.capacity != capacity {
		return candidateLiquidityBelief{
			capacity: capacity,
		}
	}

	conf := candidateBeliefConfidence(b, now)
	if conf == 0 {
		return candidateLiquidityBelief{
			capacity: capacity,
		}
	}

	b.conf = conf

	// Bounds become hints rather than permanent facts after substantial age.
	if now.Sub(b.updatedAt) > 20*time.Minute {
		b.lowerOK = 0
		b.upperFail = 0
	}

	return candidateNormalizeBelief(b, capacity)
}

func candidateSnapshot(
	edge *candidateEdge) candidateLiquidityBelief {

	candidateKnowledge.RLock()
	b, ok := candidateKnowledge.beliefs[edge.key]
	candidateKnowledge.RUnlock()

	if !ok || b.capacity != edge.capacity {
		return candidateLiquidityBelief{
			capacity: edge.capacity,
		}
	}

	return b
}

func candidateStorePair(
	key candidateEdgeKey, forward candidateLiquidityBelief,
	capacity lnwire.MilliSatoshi) {

	forward = candidateNormalizeBelief(forward, capacity)
	candidateKnowledge.beliefs[key] = forward

	reverseKey := candidateReverseKey(key)
	reverse := candidateKnowledge.beliefs[reverseKey]
	reverse = candidatePrepareObservation(
		reverse, capacity, forward.updatedAt,
	)

	reverse.updatedAt = forward.updatedAt
	reverse.conf = math.Max(reverse.conf, forward.conf*0.88)
	reverse.estimate = capacity - forward.estimate

	if forward.upperFail != 0 {
		reverse.lowerOK = capacity - forward.upperFail + 1
	}
	if forward.lowerOK != 0 {
		reverse.upperFail = capacity - forward.lowerOK + 1
	}

	candidateKnowledge.beliefs[reverseKey] =
		candidateNormalizeBelief(reverse, capacity)
}

func candidateRecordPass(
	edge *candidateEdge, amt lnwire.MilliSatoshi, now time.Time) {

	if edge == nil || amt <= 0 {
		return
	}

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	b := candidatePrepareObservation(
		candidateKnowledge.beliefs[edge.key],
		edge.capacity, now,
	)

	if amt > b.lowerOK {
		b.lowerOK = amt
	}
	if b.upperFail != 0 && amt >= b.upperFail {
		b.upperFail = 0
	}

	highEstimate := edge.capacity * 9 / 10
	if highEstimate < amt {
		highEstimate = amt
	}
	if b.estimate < highEstimate {
		b.estimate = highEstimate
	}

	b.conf = math.Max(b.conf, 0.92)
	b.updatedAt = now
	candidateStorePair(edge.key, b, edge.capacity)
}

func candidateRecordFailure(
	edge *candidateEdge, amt lnwire.MilliSatoshi, now time.Time) {

	if edge == nil || amt <= 0 {
		return
	}

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	b := candidatePrepareObservation(
		candidateKnowledge.beliefs[edge.key],
		edge.capacity, now,
	)

	if b.upperFail == 0 || amt < b.upperFail {
		b.upperFail = amt
	}
	if b.lowerOK >= amt {
		b.lowerOK = amt - 1
	}

	lowEstimate := amt / 16
	capFloor := edge.capacity / 500
	if capFloor < 1 {
		capFloor = 1
	}
	if lowEstimate > capFloor {
		lowEstimate = capFloor
	}
	if lowEstimate < b.lowerOK {
		lowEstimate = b.lowerOK
	}
	if b.estimate == 0 || lowEstimate < b.estimate {
		b.estimate = lowEstimate
	}

	b.conf = math.Max(b.conf, 0.98)
	b.updatedAt = now
	candidateStorePair(edge.key, b, edge.capacity)
}

func candidateRecordSettlement(
	edge *candidateEdge, amt lnwire.MilliSatoshi, now time.Time) {

	if edge == nil || amt <= 0 {
		return
	}

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	b := candidatePrepareObservation(
		candidateKnowledge.beliefs[edge.key],
		edge.capacity, now,
	)

	estimate := b.estimate
	if estimate < amt {
		estimate = edge.capacity * 9 / 10
		if estimate < amt {
			estimate = amt
		}
	}

	b.estimate = estimate - amt
	if b.lowerOK > amt {
		b.lowerOK -= amt
	} else {
		b.lowerOK = 0
	}
	if b.upperFail > amt {
		b.upperFail -= amt
	} else {
		b.upperFail = 0
	}

	b.conf = math.Max(b.conf, 0.94)
	b.updatedAt = now
	candidateStorePair(edge.key, b, edge.capacity)
}

type candidateRouter struct {
	view   routing.SimNetworkView
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edges         map[candidateEdgeKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	sessionLower   map[candidateEdgeKey]lnwire.MilliSatoshi
	sessionFailed  map[candidateEdgeKey]lnwire.MilliSatoshi
	sessionBlocked map[candidateEdgeKey]bool
	sessionPenalty map[candidateEdgeKey]float64
	edgeUses       map[candidateEdgeKey]uint32

	attempts uint32
}

func newCandidateRouter(
	view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	if view == nil {
		return nil, errors.New("network view is nil")
	}
	if spec == nil {
		return nil, errors.New("payment specification is nil")
	}
	if spec.Amount <= 0 {
		return nil, errors.New("payment amount must be positive")
	}
	if source == spec.Target {
		return nil, errors.New("source is payment target")
	}

	r := &candidateRouter{
		view:           view,
		source:         source,
		spec:           spec,
		incomingEdges:  make(map[route.Vertex][]*candidateEdge),
		edges:          make(map[candidateEdgeKey]*candidateEdge),
		localBalances:  make(map[uint64]lnwire.MilliSatoshi),
		sessionLower:   make(map[candidateEdgeKey]lnwire.MilliSatoshi),
		sessionFailed:  make(map[candidateEdgeKey]lnwire.MilliSatoshi),
		sessionBlocked: make(map[candidateEdgeKey]bool),
		sessionPenalty: make(map[candidateEdgeKey]float64),
		edgeUses:       make(map[candidateEdgeKey]uint32),
	}

	for chanID, balance := range localBalances {
		r.localBalances[chanID] = balance
	}

	ctx := context.Background()
	seen := map[route.Vertex]bool{source: true}
	queue := []route.Vertex{source}

	for len(queue) != 0 {
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

				r.incomingEdges[key.to] = append(
					r.incomingEdges[key.to], edge,
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

	return r, nil
}

func candidateClampProbability(probability float64) float64 {
	if probability < 0.005 {
		return 0.005
	}
	if probability > 0.995 {
		return 0.995
	}

	return probability
}

func candidatePriorProbability(
	amt, capacity lnwire.MilliSatoshi) float64 {

	if capacity <= 0 || amt <= 0 || amt > capacity {
		return 0
	}

	ratio := float64(amt) / float64(capacity)
	lowMode := 0.48 * math.Exp(-ratio/0.025)
	highMode := 0.50 /
		(1 + math.Exp((ratio-0.92)/0.045))

	return candidateClampProbability(0.005 + lowMode + highMode)
}

func candidateLearnedProbability(
	b candidateLiquidityBelief, amt,
	capacity lnwire.MilliSatoshi) float64 {

	if b.lowerOK != 0 && amt <= b.lowerOK {
		return 0.995
	}
	if b.upperFail != 0 && amt >= b.upperFail {
		return 0.005
	}

	if b.estimate == 0 {
		return candidatePriorProbability(amt, capacity)
	}

	width := math.Max(float64(capacity)*0.035, 1)
	position := (float64(amt) - float64(b.estimate)) / width
	probability := 1 / (1 + math.Exp(position))

	if b.upperFail != 0 {
		lower := float64(b.lowerOK)
		upper := float64(b.upperFail)
		fraction := (float64(amt) - lower) /
			math.Max(upper-lower, 1)
		if fraction < 0 {
			fraction = 0
		}
		if fraction > 1 {
			fraction = 1
		}

		bounded := 0.005 + 0.99*math.Pow(1-fraction, 2.4)
		probability = 0.65*bounded + 0.35*probability
	}

	return candidateClampProbability(probability)
}

func (r *candidateRouter) edgeProbability(
	edge *candidateEdge, amt lnwire.MilliSatoshi) float64 {

	if r.sessionBlocked[edge.key] {
		return 0
	}

	if edge.key.from == r.source {
		if r.localBalances[edge.key.chanID] < amt {
			return 0
		}

		return 0.9995
	}

	if failedAt := r.sessionFailed[edge.key]; failedAt != 0 {
		if amt >= failedAt {
			return 0
		}

		ratio := float64(amt) / float64(failedAt)
		if ratio > 0.75 {
			return 0.006
		}
	}

	if lower := r.sessionLower[edge.key]; lower >= amt {
		return 0.998
	}

	prior := candidatePriorProbability(amt, edge.capacity)
	if prior == 0 {
		return 0
	}

	b := candidateSnapshot(edge)
	conf := candidateBeliefConfidence(b, r.view.Now())
	if conf == 0 {
		return prior
	}

	learned := candidateLearnedProbability(
		b, amt, edge.capacity,
	)
	probability := conf*learned + (1-conf)*prior

	if failedAt := r.sessionFailed[edge.key]; failedAt != 0 {
		ratio := float64(amt) / float64(failedAt)
		switch {
		case ratio > 0.55:
			probability *= 0.08
		case ratio > 0.30:
			probability *= 0.30
		case ratio > 0.12:
			probability *= 0.65
		}
	}

	return candidateClampProbability(probability)
}

type candidateQueueItem struct {
	node   route.Vertex
	amount lnwire.MilliSatoshi
	score  float64
	risk   float64
	hops   uint16
}

type candidateQueue []*candidateQueueItem

func (q candidateQueue) Len() int {
	return len(q)
}

func (q candidateQueue) Less(i, j int) bool {
	if math.Abs(q[i].score-q[j].score) > 1e-12 {
		return q[i].score < q[j].score
	}

	return q[i].amount < q[j].amount
}

func (q candidateQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

func (q *candidateQueue) Push(value any) {
	*q = append(*q, value.(*candidateQueueItem))
}

func (q *candidateQueue) Pop() any {
	old := *q
	last := old[len(old)-1]
	*q = old[:len(old)-1]

	return last
}

func (r *candidateRouter) findRoute(
	deliver lnwire.MilliSatoshi) (*route.Route, float64, error) {

	if deliver <= 0 {
		return nil, 0, errors.New("route amount must be positive")
	}

	bestScore := make(map[route.Vertex]float64)
	required := make(map[route.Vertex]lnwire.MilliSatoshi)
	next := make(map[route.Vertex]*candidateEdge)

	bestScore[r.spec.Target] = 0
	required[r.spec.Target] = deliver

	queue := &candidateQueue{}
	heap.Push(queue, &candidateQueueItem{
		node:   r.spec.Target,
		amount: deliver,
	})

	sourceRisk := 0.0
	feeScale := math.Max(float64(deliver), 1_000_000)

	for queue.Len() != 0 {
		item := heap.Pop(queue).(*candidateQueueItem)

		score, ok := bestScore[item.node]
		if !ok || item.score > score+1e-12 {
			continue
		}
		if required[item.node] != item.amount {
			continue
		}
		if item.node == r.source {
			sourceRisk = item.risk
			break
		}
		if item.hops >= candidateMaxRouteHops {
			continue
		}

		for _, edge := range r.incomingEdges[item.node] {
			if !edge.usable(item.amount) {
				continue
			}

			probability := r.edgeProbability(edge, item.amount)
			if probability <= 0 {
				continue
			}

			sending := item.amount
			fee := lnwire.MilliSatoshi(0)
			if edge.key.from != r.source {
				fee = edge.fee(item.amount)
				sending += fee
			}

			riskCost := -math.Log(probability)
			feeCost := 8 * float64(fee) / feeScale
			hopCost := 0.055
			useCost := 0.035 * math.Min(
				float64(r.edgeUses[edge.key]), 8,
			)
			penalty := r.sessionPenalty[edge.key]

			newScore := item.score + riskCost + feeCost +
				hopCost + useCost + penalty

			oldScore, exists := bestScore[edge.key.from]
			oldAmount := required[edge.key.from]
			if exists &&
				(newScore > oldScore+1e-12 ||
					(math.Abs(newScore-oldScore) <= 1e-12 &&
						sending >= oldAmount)) {

				continue
			}

			bestScore[edge.key.from] = newScore
			required[edge.key.from] = sending
			next[edge.key.from] = edge

			heap.Push(queue, &candidateQueueItem{
				node:   edge.key.from,
				amount: sending,
				score:  newScore,
				risk:   item.risk + riskCost,
				hops:   item.hops + 1,
			})
		}
	}

	if _, ok := next[r.source]; !ok {
		return nil, 0, errors.New("no route found")
	}

	rt, err := r.buildRoute(deliver, next)
	if err != nil {
		return nil, 0, err
	}

	return rt, sourceRisk, nil
}

func (r *candidateRouter) buildRoute(
	deliver lnwire.MilliSatoshi,
	next map[route.Vertex]*candidateEdge) (*route.Route, error) {

	path := make([]*candidateEdge, 0, 8)
	visited := make(map[route.Vertex]bool)

	for node := r.source; node != r.spec.Target; {
		if visited[node] {
			return nil, errors.New("cycle in selected route")
		}
		visited[node] = true

		edge, ok := next[node]
		if !ok {
			return nil, fmt.Errorf("broken path at %v", node)
		}

		path = append(path, edge)
		if len(path) > candidateMaxRouteHops {
			return nil, errors.New("selected route is too long")
		}

		node = edge.key.to
	}

	if len(path) == 0 {
		return nil, errors.New("selected route has no hops")
	}

	amounts := make([]lnwire.MilliSatoshi, len(path))
	expiries := make([]uint32, len(path))

	last := len(path) - 1
	amounts[last] = deliver
	expiries[last] = candidateFinalCltvDelta

	for i := last - 1; i >= 0; i-- {
		outgoing := path[i+1]
		amounts[i] = amounts[i+1] +
			outgoing.fee(amounts[i+1])
		expiries[i] = expiries[i+1] +
			uint32(outgoing.timeLockDelta)
	}

	hops := make([]*route.Hop, len(path))
	for i, edge := range path {
		amountToForward := deliver
		outgoingExpiry := uint32(candidateFinalCltvDelta)

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

func candidateCeilDiv(
	amt lnwire.MilliSatoshi, divisor uint32) lnwire.MilliSatoshi {

	if divisor <= 1 {
		return amt
	}

	d := lnwire.MilliSatoshi(divisor)
	return (amt + d - 1) / d
}

func candidateAppendUnique(
	amounts []lnwire.MilliSatoshi,
	amt lnwire.MilliSatoshi) []lnwire.MilliSatoshi {

	if amt <= 0 {
		return amounts
	}

	for _, existing := range amounts {
		if existing == amt {
			return amounts
		}
	}

	return append(amounts, amt)
}

func candidateShardAmounts(
	amt lnwire.MilliSatoshi,
	partsLeft uint32) []lnwire.MilliSatoshi {

	if partsLeft <= 1 {
		return []lnwire.MilliSatoshi{amt}
	}

	amounts := make([]lnwire.MilliSatoshi, 0, 20)
	amounts = candidateAppendUnique(amounts, amt)

	limit := partsLeft
	if limit > 16 {
		limit = 16
	}

	for parts := uint32(2); parts <= limit; parts++ {
		amounts = candidateAppendUnique(
			amounts, candidateCeilDiv(amt, parts),
		)
	}

	amounts = candidateAppendUnique(
		amounts, candidateCeilDiv(amt, partsLeft),
	)

	return amounts
}

func (r *candidateRouter) markRouteUsed(rt *route.Route) {
	from := rt.SourcePubKey
	for _, hop := range rt.Hops {
		key := candidateEdgeKey{
			chanID: hop.ChannelID,
			from:   from,
			to:     hop.PubKeyBytes,
		}
		r.edgeUses[key]++
		from = hop.PubKeyBytes
	}
}

func (r *candidateRouter) RequestRoute(
	amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt <= 0 {
		return nil, errors.New("remaining amount must be positive")
	}
	if r.attempts >= candidateAttemptLimit {
		return nil, errors.New("routing attempt budget exhausted")
	}

	maxParts := r.spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}
	if inFlightHtlcs >= maxParts {
		return nil, errors.New("maximum payment parts reached")
	}

	partsLeft := maxParts - inFlightHtlcs
	shards := candidateShardAmounts(amt, partsLeft)
	minimum := candidateCeilDiv(amt, partsLeft)

	var bestRoute *route.Route
	bestUtility := math.Inf(-1)

	for _, shard := range shards {
		rt, logRisk, err := r.findRoute(shard)
		if err != nil {
			continue
		}

		probability := math.Exp(-logRisk)
		progress := math.Log1p(
			float64(shard) / math.Max(float64(minimum), 1),
		)
		fee := rt.TotalAmount - shard
		feePenalty := 6 * float64(fee) /
			math.Max(float64(shard), 1)

		utility := math.Log(math.Max(probability, 1e-12)) +
			0.48*progress - feePenalty

		if bestRoute == nil || utility > bestUtility {
			bestRoute = rt
			bestUtility = utility
		}

		if probability >= 0.55 && shard >= amt/2 {
			bestRoute = rt
			break
		}
	}

	if bestRoute == nil {
		return nil, errors.New("no route found")
	}

	r.attempts++
	r.markRouteUsed(bestRoute)

	return bestRoute, nil
}

func (r *candidateRouter) routeData(
	rt *route.Route) ([]candidateEdgeKey,
	[]lnwire.MilliSatoshi) {

	keys := make([]candidateEdgeKey, len(rt.Hops))
	amounts := make([]lnwire.MilliSatoshi, len(rt.Hops))

	from := rt.SourcePubKey
	for i, hop := range rt.Hops {
		keys[i] = candidateEdgeKey{
			chanID: hop.ChannelID,
			from:   from,
			to:     hop.PubKeyBytes,
		}

		if i == 0 {
			amounts[i] = rt.TotalAmount
		} else {
			amounts[i] = rt.Hops[i-1].AmtToForward
		}

		from = hop.PubKeyBytes
	}

	return keys, amounts
}

func candidateFailureIndex(
	rt *route.Route, source route.Vertex) int {

	if source == rt.SourcePubKey {
		return 0
	}

	for i, hop := range rt.Hops {
		if hop.PubKeyBytes == source {
			return i + 1
		}
	}

	return -1
}

func (r *candidateRouter) recordSessionPass(
	key candidateEdgeKey, amt lnwire.MilliSatoshi) {

	if amt > r.sessionLower[key] {
		r.sessionLower[key] = amt
	}

	if failed := r.sessionFailed[key]; failed != 0 && amt >= failed {
		delete(r.sessionFailed, key)
	}

	r.sessionPenalty[key] *= 0.20
}

func (r *candidateRouter) recordSessionFailure(
	key candidateEdgeKey, amt lnwire.MilliSatoshi) {

	failed := r.sessionFailed[key]
	if failed == 0 || amt < failed {
		r.sessionFailed[key] = amt
	}

	if r.sessionLower[key] >= amt {
		r.sessionLower[key] = amt - 1
	}

	r.sessionPenalty[key] = math.Min(
		r.sessionPenalty[key]+1.25, 6,
	)
}

func (r *candidateRouter) recordSessionSettlement(
	key candidateEdgeKey, amt,
	capacity lnwire.MilliSatoshi) {

	if lower := r.sessionLower[key]; lower > amt {
		r.sessionLower[key] = lower - amt
	} else {
		delete(r.sessionLower, key)
	}

	if failed := r.sessionFailed[key]; failed > amt {
		r.sessionFailed[key] = failed - amt
	} else {
		delete(r.sessionFailed, key)
	}

	r.sessionPenalty[key] *= 0.15

	reverse := candidateReverseKey(key)
	reverseLower := r.sessionLower[reverse] + amt
	if reverseLower > capacity {
		reverseLower = capacity
	}
	r.sessionLower[reverse] = reverseLower
}

func (r *candidateRouter) penalizeUnknownRoute(
	keys []candidateEdgeKey) {

	for i, key := range keys {
		if key.from == r.source {
			continue
		}

		penalty := 0.45
		if i > len(keys)/2 {
			penalty = 0.60
		}
		r.sessionPenalty[key] = math.Min(
			r.sessionPenalty[key]+penalty, 4,
		)
	}
}

func (r *candidateRouter) ReportAttempt(
	_ uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if rt == nil {
		return errors.New("reported route is nil")
	}

	keys, amounts := r.routeData(rt)
	if len(keys) == 0 {
		return nil
	}

	now := r.view.Now()

	if result.Failure == nil {
		for i, key := range keys {
			edge := r.edges[key]
			if edge == nil {
				continue
			}

			candidateRecordSettlement(edge, amounts[i], now)
			r.recordSessionSettlement(
				key, amounts[i], edge.capacity,
			)
		}

		first := keys[0]
		if balance := r.localBalances[first.chanID];
			balance > amounts[0] {

			r.localBalances[first.chanID] =
				balance - amounts[0]
		} else {
			r.localBalances[first.chanID] = 0
		}

		return nil
	}

	failIndex := candidateFailureIndex(
		rt, result.FailureSource,
	)

	if failIndex >= 0 {
		prefixEnd := failIndex
		if prefixEnd > len(keys) {
			prefixEnd = len(keys)
		}

		for i := 0; i < prefixEnd; i++ {
			edge := r.edges[keys[i]]
			if edge == nil || edge.key.from == r.source {
				continue
			}

			candidateRecordPass(edge, amounts[i], now)
			r.recordSessionPass(keys[i], amounts[i])
		}
	}

	code := result.Failure.Code()

	if failIndex >= 0 && failIndex < len(keys) {
		key := keys[failIndex]
		edge := r.edges[key]
		if edge == nil {
			return nil
		}

		switch code {
		case lnwire.CodeTemporaryChannelFailure:
			candidateRecordFailure(
				edge, amounts[failIndex], now,
			)
			r.recordSessionFailure(
				key, amounts[failIndex],
			)

		case lnwire.CodeFeeInsufficient,
			lnwire.CodeIncorrectCltvExpiry:

			r.sessionBlocked[key] = true
			r.sessionPenalty[key] = 20

		default:
			r.sessionBlocked[key] = true
		}

		return nil
	}

	// Unknown-source failures contain no reliable channel attribution.
	// Penalizing the route still forces exploration without poisoning
	// persistent channel beliefs.
	r.penalizeUnknownRoute(keys)

	return nil
}