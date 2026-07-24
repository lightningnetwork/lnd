package main

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	candidateFinalCltvDelta = 40
	candidateAttemptLimit   = 48
	candidateLabelLimit     = 8
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

func (e *candidateEdge) usable(
	amt lnwire.MilliSatoshi) bool {

	if amt <= 0 || amt < e.minHTLC || amt > e.capacity {
		return false
	}
	if e.maxHTLC != 0 && amt > e.maxHTLC {
		return false
	}

	return true
}

type candidateLiquidityState struct {
	capacity  lnwire.MilliSatoshi
	lowerOK   lnwire.MilliSatoshi
	upperFail lnwire.MilliSatoshi
	estimate  lnwire.MilliSatoshi
	conf      float64
	known     bool
	failures  uint32
	successes uint32
}

var candidateKnowledge = struct {
	sync.Mutex
	states map[candidateEdgeKey]*candidateLiquidityState
}{
	states: make(map[candidateEdgeKey]*candidateLiquidityState),
}

func candidateMutableStateLocked(
	key candidateEdgeKey,
	capacity lnwire.MilliSatoshi) *candidateLiquidityState {

	state := candidateKnowledge.states[key]
	if state == nil || state.capacity != capacity {
		state = &candidateLiquidityState{
			capacity: capacity,
		}
		candidateKnowledge.states[key] = state
	}

	return state
}

func candidateStateSnapshot(
	edge *candidateEdge) candidateLiquidityState {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	state := candidateKnowledge.states[edge.key]
	if state == nil || state.capacity != edge.capacity {
		return candidateLiquidityState{
			capacity: edge.capacity,
		}
	}

	return *state
}

func candidateRecordProbe(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	state := candidateMutableStateLocked(
		edge.key, edge.capacity,
	)

	if amt > state.lowerOK {
		state.lowerOK = amt
	}
	if state.upperFail != 0 && state.lowerOK >= state.upperFail {
		state.upperFail = 0
	}

	richEstimate := edge.capacity * 9 / 10
	if richEstimate < amt {
		richEstimate = amt
	}
	if !state.known || state.estimate < richEstimate {
		state.estimate = richEstimate
	}

	state.known = true
	state.conf = math.Max(state.conf, 0.82)
	state.successes++
	if state.failures > 0 {
		state.failures--
	}
}

func candidateRecordFailure(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	state := candidateMutableStateLocked(
		edge.key, edge.capacity,
	)

	if state.upperFail == 0 || amt < state.upperFail {
		state.upperFail = amt
	}
	if state.lowerOK >= amt {
		state.lowerOK = 0
	}

	dryEstimate := amt / 10
	if !state.known || state.estimate > dryEstimate {
		state.estimate = dryEstimate
	}

	state.known = true
	state.conf = math.Max(state.conf, 0.92)
	state.failures++
}

func candidateRecordSettlement(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	forward := candidateMutableStateLocked(
		edge.key, edge.capacity,
	)

	estimate := forward.estimate
	if !forward.known {
		estimate = edge.capacity * 9 / 10
	}
	if estimate < amt {
		estimate = amt
	}

	forward.estimate = estimate - amt
	if forward.lowerOK > amt {
		forward.lowerOK -= amt
	} else {
		forward.lowerOK = 0
	}
	if forward.upperFail > amt {
		forward.upperFail -= amt
	} else {
		forward.upperFail = 0
	}

	forward.known = true
	forward.conf = math.Max(forward.conf, 0.86)
	forward.successes++
	if forward.failures > 0 {
		forward.failures--
	}

	reverseKey := candidateEdgeKey{
		chanID: edge.key.chanID,
		from:   edge.key.to,
		to:     edge.key.from,
	}
	reverse := candidateMutableStateLocked(
		reverseKey, edge.capacity,
	)

	if reverse.known {
		reverse.estimate += amt
	} else {
		reverse.estimate = amt
	}
	if reverse.estimate > edge.capacity {
		reverse.estimate = edge.capacity
	}

	reverse.lowerOK += amt
	if reverse.lowerOK > edge.capacity {
		reverse.lowerOK = edge.capacity
	}
	if reverse.upperFail != 0 {
		reverse.upperFail += amt
		if reverse.upperFail > edge.capacity {
			reverse.upperFail = edge.capacity
		}
	}

	reverse.known = true
	reverse.conf = math.Max(reverse.conf, 0.90)
}

type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edges         map[candidateEdgeKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	sessionPenalty map[candidateEdgeKey]float64
	sessionBlocked map[candidateEdgeKey]bool
	reserved       map[candidateEdgeKey]lnwire.MilliSatoshi
	attempts       uint32
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	if spec == nil {
		return nil, errors.New("payment specification is nil")
	}

	router := &candidateRouter{
		source:          source,
		spec:            spec,
		incomingEdges:   make(map[route.Vertex][]*candidateEdge),
		edges:           make(map[candidateEdgeKey]*candidateEdge),
		localBalances:   make(map[uint64]lnwire.MilliSatoshi),
		sessionPenalty:  make(map[candidateEdgeKey]float64),
		sessionBlocked:  make(map[candidateEdgeKey]bool),
		reserved:        make(map[candidateEdgeKey]lnwire.MilliSatoshi),
	}

	for chanID, balance := range localBalances {
		router.localBalances[chanID] = balance
	}

	ctx := context.Background()
	seen := make(map[route.Vertex]bool)
	queue := []route.Vertex{source}
	seen[source] = true

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
				if _, exists := router.edges[key]; exists {
					return nil
				}

				edge := &candidateEdge{
					key: key,
					capacity: lnwire.NewMSatFromSatoshis(
						ch.Capacity,
					),
					baseFeeMsat: policy.FeeBaseMSat,
					feeRatePPM: policy.
						FeeProportionalMillionths,
					timeLockDelta: policy.TimeLockDelta,
					minHTLC:       policy.MinHTLC,
				}
				if policy.HasMaxHTLC {
					edge.maxHTLC = policy.MaxHTLC
				}

				router.incomingEdges[key.to] = append(
					router.incomingEdges[key.to], edge,
				)
				router.edges[key] = edge

				return nil
			},
			func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	return router, nil
}

func candidatePriorSurvival(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if amt <= 0 {
		return 1
	}
	if edge.capacity <= 0 || amt > edge.capacity {
		return 0
	}

	ratio := float64(amt) / float64(edge.capacity)

	dry := 0.47 * math.Exp(-ratio/0.035)
	rich := 0.50 /
		(1 + math.Exp((ratio-0.94)/0.025))
	middle := 0.03 * math.Max(1-ratio, 0)

	probability := dry + rich + middle
	if probability > 0.995 {
		probability = 0.995
	}
	if probability < 0.001 {
		probability = 0.001
	}

	return probability
}

func candidateBaseProbability(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if amt <= 0 {
		return 1
	}
	if amt > edge.capacity {
		return 0
	}

	state := candidateStateSnapshot(edge)

	if state.upperFail != 0 && amt >= state.upperFail {
		return 0
	}
	if state.lowerOK >= amt {
		return 0.998
	}

	if state.upperFail != 0 {
		lowerSurvival := candidatePriorSurvival(
			edge, state.lowerOK,
		)
		upperSurvival := candidatePriorSurvival(
			edge, state.upperFail,
		)
		amountSurvival := candidatePriorSurvival(edge, amt)

		denominator := lowerSurvival - upperSurvival
		numerator := amountSurvival - upperSurvival
		if denominator > 1e-9 {
			probability := numerator / denominator
			if probability > 0.998 {
				probability = 0.998
			}
			if probability < 0 {
				probability = 0
			}

			return probability
		}
	}

	if state.lowerOK > 0 {
		amountSurvival := candidatePriorSurvival(edge, amt)
		lowerSurvival := candidatePriorSurvival(
			edge, state.lowerOK,
		)
		if lowerSurvival > 1e-9 {
			probability := amountSurvival / lowerSurvival
			if probability > 0.998 {
				probability = 0.998
			}
			if probability < 0.001 {
				probability = 0.001
			}

			return probability
		}
	}

	prior := candidatePriorSurvival(edge, amt)
	if !state.known {
		return prior
	}

	if state.estimate >= amt {
		margin := float64(state.estimate-amt) /
			math.Max(float64(edge.capacity), 1)
		probability := 0.72 + 0.25*state.conf +
			0.03*math.Min(margin, 1)
		if probability > 0.998 {
			probability = 0.998
		}

		return probability
	}

	shortfall := float64(amt-state.estimate) /
		math.Max(float64(edge.capacity), 1)
	probability := prior *
		(1 - 0.72*state.conf) *
		math.Exp(-4*shortfall)

	if state.successes > state.failures {
		probability += 0.04
	}
	if probability > 0.65 {
		probability = 0.65
	}
	if probability < 0.001 {
		probability = 0.001
	}

	return probability
}

func (r *candidateRouter) edgeProbability(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if r.sessionBlocked[edge.key] {
		return 0
	}

	reserved := r.reserved[edge.key]
	if reserved > edge.capacity-amt {
		return 0
	}
	required := reserved + amt

	if edge.key.from == r.source {
		balance := r.localBalances[edge.key.chanID]
		if reserved > balance || balance-reserved < amt {
			return 0
		}

		return 1
	}

	totalProbability := candidateBaseProbability(
		edge, required,
	)
	if totalProbability <= 0 {
		return 0
	}
	if reserved == 0 {
		return totalProbability
	}

	reservedProbability := candidateBaseProbability(
		edge, reserved,
	)
	if reservedProbability <= 0 {
		return 0
	}

	probability := totalProbability / reservedProbability
	if probability > 0.998 {
		probability = 0.998
	}
	if probability < 0.001 {
		probability = 0.001
	}

	return probability
}

type candidatePathLabel struct {
	node    route.Vertex
	amount  lnwire.MilliSatoshi
	score   float64
	logRisk float64
	hops    int

	edge   *candidateEdge
	next   *candidatePathLabel
	active bool
}

type candidatePathQueue []*candidatePathLabel

func (q candidatePathQueue) Len() int {
	return len(q)
}

func (q candidatePathQueue) Less(i, j int) bool {
	if math.Abs(q[i].score-q[j].score) > 1e-12 {
		return q[i].score < q[j].score
	}
	if q[i].hops != q[j].hops {
		return q[i].hops < q[j].hops
	}

	return q[i].amount < q[j].amount
}

func (q candidatePathQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

func (q *candidatePathQueue) Push(value any) {
	*q = append(*q, value.(*candidatePathLabel))
}

func (q *candidatePathQueue) Pop() any {
	old := *q
	last := len(old) - 1
	item := old[last]
	*q = old[:last]

	return item
}

func candidateLabelContains(
	label *candidatePathLabel,
	node route.Vertex) bool {

	for current := label; current != nil; current = current.next {
		if current.node == node {
			return true
		}
	}

	return false
}

func candidateDominates(
	left, right *candidatePathLabel) bool {

	return left.active &&
		left.score <= right.score+1e-12 &&
		left.amount <= right.amount &&
		left.hops <= right.hops
}

func candidateInsertLabel(
	labels map[route.Vertex][]*candidatePathLabel,
	label *candidatePathLabel,
	deliver lnwire.MilliSatoshi) bool {

	current := labels[label.node]
	for _, existing := range current {
		if candidateDominates(existing, label) {
			return false
		}
	}

	for _, existing := range current {
		if candidateDominates(label, existing) {
			existing.active = false
		}
	}

	label.active = true
	current = append(current, label)

	activeCount := 0
	for _, existing := range current {
		if existing.active {
			activeCount++
		}
	}

	if activeCount > candidateLabelLimit {
		var worst *candidatePathLabel
		worstRank := math.Inf(-1)

		for _, existing := range current {
			if !existing.active {
				continue
			}

			amountRatio := float64(existing.amount) /
				math.Max(float64(deliver), 1)
			rank := existing.score +
				0.04*float64(existing.hops) +
				0.08*math.Log(math.Max(amountRatio, 1))

			if worst == nil || rank > worstRank {
				worst = existing
				worstRank = rank
			}
		}

		worst.active = false
	}

	labels[label.node] = current

	return label.active
}

func (r *candidateRouter) findRouteWithHopLimit(
	deliver lnwire.MilliSatoshi,
	maxHops int) (*route.Route, float64, float64, error) {

	labels := make(map[route.Vertex][]*candidatePathLabel)
	queue := &candidatePathQueue{}

	start := &candidatePathLabel{
		node:    r.spec.Target,
		amount:  deliver,
		active:  true,
		score:   0,
		logRisk: 0,
	}
	labels[start.node] = []*candidatePathLabel{start}
	heap.Push(queue, start)

	for queue.Len() != 0 {
		label := heap.Pop(queue).(*candidatePathLabel)
		if !label.active {
			continue
		}

		if label.node == r.source {
			path := make([]*candidateEdge, 0, label.hops)
			for current := label; current.edge != nil;
				current = current.next {

				path = append(path, current.edge)
			}

			rt, err := r.buildRoute(deliver, path)
			if err != nil {
				return nil, 0, 0, err
			}

			return rt, label.logRisk, label.score, nil
		}

		if label.hops >= maxHops {
			continue
		}

		for _, edge := range r.incomingEdges[label.node] {
			if candidateLabelContains(label, edge.key.from) {
				continue
			}

			amountOver := label.amount
			if !edge.usable(amountOver) {
				continue
			}

			probability := r.edgeProbability(edge, amountOver)
			if probability <= 0 {
				continue
			}

			sending := amountOver
			fee := lnwire.MilliSatoshi(0)
			if edge.key.from != r.source {
				fee = edge.fee(amountOver)
				if fee < 0 || sending > math.MaxInt64-fee {
					continue
				}
				sending += fee
			}

			logRisk := -math.Log(probability)
			feePenalty := 6 * float64(fee) /
				math.Max(float64(deliver), 1)
			utilization := float64(amountOver) /
				math.Max(float64(edge.capacity), 1)
			capacityPenalty := 0.04 *
				math.Sqrt(math.Max(utilization, 0))
			hopPenalty := 0.018

			nextLabel := &candidatePathLabel{
				node:    edge.key.from,
				amount:  sending,
				score: label.score + logRisk +
					r.sessionPenalty[edge.key] +
					feePenalty + capacityPenalty +
					hopPenalty,
				logRisk: label.logRisk + logRisk,
				hops:    label.hops + 1,
				edge:    edge,
				next:    label,
			}

			if !candidateInsertLabel(
				labels, nextLabel, deliver,
			) {
				continue
			}

			heap.Push(queue, nextLabel)
		}
	}

	return nil, 0, 0, errors.New("no route found")
}

func (r *candidateRouter) findRoute(
	deliver lnwire.MilliSatoshi) (*route.Route, float64, float64, error) {

	if deliver <= 0 {
		return nil, 0, 0, errors.New(
			"route amount must be positive",
		)
	}
	if r.source == r.spec.Target {
		return nil, 0, 0, errors.New(
			"source is payment target",
		)
	}

	hopLimits := [...]int{20, 32, 64}
	var lastErr error

	for _, limit := range hopLimits {
		rt, risk, score, err := r.findRouteWithHopLimit(
			deliver, limit,
		)
		if err == nil {
			return rt, risk, score, nil
		}
		lastErr = err
	}

	return nil, 0, 0, lastErr
}

func (r *candidateRouter) buildRoute(
	deliver lnwire.MilliSatoshi,
	path []*candidateEdge) (*route.Route, error) {

	if len(path) == 0 {
		return nil, errors.New("selected route has no hops")
	}

	amounts := make([]lnwire.MilliSatoshi, len(path))
	expiries := make([]uint32, len(path))

	last := len(path) - 1
	amounts[last] = deliver
	expiries[last] = candidateFinalCltvDelta

	for i := last - 1; i >= 0; i-- {
		nextEdge := path[i+1]
		fee := nextEdge.fee(amounts[i+1])
		if fee < 0 || amounts[i+1] > math.MaxInt64-fee {
			return nil, errors.New("route amount overflow")
		}

		amounts[i] = amounts[i+1] + fee
		expiries[i] = expiries[i+1] +
			uint32(nextEdge.timeLockDelta)
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
	amt lnwire.MilliSatoshi,
	divisor uint32) lnwire.MilliSatoshi {

	if divisor <= 1 {
		return amt
	}

	d := lnwire.MilliSatoshi(divisor)
	result := amt / d
	if amt%d != 0 {
		result++
	}

	return result
}

func candidateShardAmounts(
	amt lnwire.MilliSatoshi,
	partsLeft uint32) []lnwire.MilliSatoshi {

	if partsLeft <= 1 {
		return []lnwire.MilliSatoshi{amt}
	}

	minimum := candidateCeilDiv(amt, partsLeft)
	seen := make(map[lnwire.MilliSatoshi]bool)

	add := func(value lnwire.MilliSatoshi) {
		if value < minimum || value > amt || value <= 0 {
			return
		}
		seen[value] = true
	}

	limit := partsLeft
	if limit > 12 {
		limit = 12
	}
	for parts := uint32(1); parts <= limit; parts++ {
		add(candidateCeilDiv(amt, parts))
	}

	fractions := [][2]int64{
		{7, 8},
		{3, 4},
		{2, 3},
		{3, 5},
		{2, 5},
	}
	for _, fraction := range fractions {
		numerator := lnwire.MilliSatoshi(fraction[0])
		denominator := lnwire.MilliSatoshi(fraction[1])
		value := (amt / denominator) * numerator
		remainder := (amt % denominator) * numerator
		value += remainder / denominator
		if remainder%denominator != 0 {
			value++
		}
		add(value)
	}

	add(minimum)
	add(amt)

	amounts := make([]lnwire.MilliSatoshi, 0, len(seen))
	for value := range seen {
		amounts = append(amounts, value)
	}

	sort.Slice(amounts, func(i, j int) bool {
		return amounts[i] > amounts[j]
	})

	return amounts
}

func (r *candidateRouter) reserveRoute(
	rt *route.Route) {

	for i, edge := range r.routeEdges(rt) {
		if edge == nil {
			continue
		}

		r.reserved[edge.key] += candidateRouteAmount(rt, i)
	}
}

func (r *candidateRouter) releaseRoute(
	rt *route.Route) {

	for i, edge := range r.routeEdges(rt) {
		if edge == nil {
			continue
		}

		amount := candidateRouteAmount(rt, i)
		current := r.reserved[edge.key]
		if current > amount {
			r.reserved[edge.key] = current - amount
		} else {
			delete(r.reserved, edge.key)
		}
	}
}

func (r *candidateRouter) RequestRoute(
	amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt <= 0 {
		return nil, errors.New("remaining amount must be positive")
	}
	if r.attempts+inFlightHtlcs >= candidateAttemptLimit {
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
		rt, logRisk, searchScore, err := r.findRoute(shard)
		if err != nil {
			continue
		}

		progress := math.Log(
			math.Max(float64(shard)/float64(minimum), 1),
		)
		fee := rt.TotalAmount - shard
		feePenalty := 4 * float64(fee) /
			math.Max(float64(shard), 1)
		nonRiskCost := math.Max(searchScore-logRisk, 0)

		utility := -logRisk +
			0.50*progress -
			feePenalty -
			0.35*nonRiskCost

		if bestRoute == nil || utility > bestUtility {
			bestRoute = rt
			bestUtility = utility
		}
	}

	if bestRoute == nil {
		return nil, errors.New("no route found")
	}

	r.reserveRoute(bestRoute)

	return bestRoute, nil
}

func (r *candidateRouter) routeEdges(
	rt *route.Route) []*candidateEdge {

	edges := make([]*candidateEdge, len(rt.Hops))
	from := rt.SourcePubKey

	for i, hop := range rt.Hops {
		key := candidateEdgeKey{
			chanID: hop.ChannelID,
			from:   from,
			to:     hop.PubKeyBytes,
		}
		edges[i] = r.edges[key]
		from = hop.PubKeyBytes
	}

	return edges
}

func candidateRouteAmount(
	rt *route.Route,
	channelIndex int) lnwire.MilliSatoshi {

	if channelIndex == 0 {
		return rt.TotalAmount
	}

	return rt.Hops[channelIndex-1].AmtToForward
}

func candidateFailureIndex(
	rt *route.Route,
	source route.Vertex) int {

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

func (r *candidateRouter) ReportAttempt(
	attemptID uint64,
	rt *route.Route,
	result routing.SimHtlcResult) error {

	_ = attemptID
	r.attempts++

	if rt == nil {
		return errors.New("reported route is nil")
	}

	r.releaseRoute(rt)
	edges := r.routeEdges(rt)

	if result.Failure == nil {
		for i, edge := range edges {
			if edge == nil {
				continue
			}

			candidateRecordSettlement(
				edge, candidateRouteAmount(rt, i),
			)
			delete(r.sessionPenalty, edge.key)
			delete(r.sessionBlocked, edge.key)
		}

		if len(rt.Hops) != 0 {
			firstChan := rt.Hops[0].ChannelID
			spent := rt.TotalAmount
			if r.localBalances[firstChan] > spent {
				r.localBalances[firstChan] -= spent
			} else {
				r.localBalances[firstChan] = 0
			}
		}

		return nil
	}

	failIndex := candidateFailureIndex(
		rt, result.FailureSource,
	)

	if failIndex >= 0 {
		prefixEnd := failIndex
		if prefixEnd > len(edges) {
			prefixEnd = len(edges)
		}

		for i := 0; i < prefixEnd; i++ {
			edge := edges[i]
			if edge == nil || edge.key.from == r.source {
				continue
			}

			candidateRecordProbe(
				edge, candidateRouteAmount(rt, i),
			)
			r.sessionPenalty[edge.key] *= 0.5
		}
	}

	code := result.Failure.Code()
	if failIndex >= 0 && failIndex < len(edges) {
		edge := edges[failIndex]
		if edge == nil {
			return nil
		}

		switch code {
		case lnwire.CodeTemporaryChannelFailure:
			candidateRecordFailure(
				edge, candidateRouteAmount(rt, failIndex),
			)
			r.sessionPenalty[edge.key] += 0.22

		case lnwire.CodeFeeInsufficient,
			lnwire.CodeIncorrectCltvExpiry:

			r.sessionBlocked[edge.key] = true

		default:
			r.sessionBlocked[edge.key] = true
		}

		return nil
	}

	penalized := 0
	for _, edge := range edges {
		if edge != nil && edge.key.from != r.source {
			penalized++
		}
	}
	if penalized == 0 {
		return nil
	}

	increment := 1.20 / float64(penalized)
	for _, edge := range edges {
		if edge == nil || edge.key.from == r.source {
			continue
		}

		r.sessionPenalty[edge.key] += increment
	}

	return nil
}

func (r *candidateRouter) String() string {
	return fmt.Sprintf(
		"candidateRouter(source=%v,target=%v)",
		r.source, r.spec.Target,
	)
}