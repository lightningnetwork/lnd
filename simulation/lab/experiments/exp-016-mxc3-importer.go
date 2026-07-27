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

const (
	candidateFinalCltvDelta = 40
	candidateMaxRouteHops   = 24
	candidateMaxLabels      = 24
	candidateSearchLimit    = 120000
	candidateAttemptLimit   = 80
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

	return e.baseFeeMsat + amt*e.feeRatePPM/1_000_000
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
	lowerOK   lnwire.MilliSatoshi
	upperFail lnwire.MilliSatoshi
	estimate  lnwire.MilliSatoshi

	confidence float64
	failures   uint32
	successes  uint32
	mode       int8
	known      bool
}

var candidateKnowledge = struct {
	sync.Mutex
	states map[candidateEdgeKey]*candidateLiquidityState
}{
	states: make(map[candidateEdgeKey]*candidateLiquidityState),
}

func candidateMutableStateLocked(
	key candidateEdgeKey) *candidateLiquidityState {

	state := candidateKnowledge.states[key]
	if state == nil {
		state = &candidateLiquidityState{}
		candidateKnowledge.states[key] = state
	}

	return state
}

func candidateReverseKey(
	key candidateEdgeKey) candidateEdgeKey {

	return candidateEdgeKey{
		chanID: key.chanID,
		from:   key.to,
		to:     key.from,
	}
}

func candidateNormalizeState(
	state *candidateLiquidityState,
	capacity lnwire.MilliSatoshi) {

	if state.lowerOK < 0 {
		state.lowerOK = 0
	}
	if state.lowerOK > capacity {
		state.lowerOK = capacity
	}

	if state.upperFail < 0 || state.upperFail > capacity {
		state.upperFail = 0
	}
	if state.upperFail != 0 && state.lowerOK >= state.upperFail {
		state.upperFail = 0
	}

	if state.estimate < state.lowerOK {
		state.estimate = state.lowerOK
	}
	if state.estimate > capacity {
		state.estimate = capacity
	}
	if state.estimate < 0 {
		state.estimate = 0
	}
	if state.upperFail != 0 && state.estimate >= state.upperFail {
		state.estimate = state.upperFail - 1
		if state.estimate < state.lowerOK {
			state.estimate = state.lowerOK
		}
	}

	if capacity > 0 {
		switch {
		case state.estimate <= capacity/50:
			state.mode = -1

		case state.estimate >= capacity*49/50:
			state.mode = 1
		}
	}
}

func candidateStateSnapshot(
	edge *candidateEdge) candidateLiquidityState {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	state := candidateKnowledge.states[edge.key]
	if state == nil {
		return candidateLiquidityState{}
	}

	snapshot := *state
	candidateNormalizeState(&snapshot, edge.capacity)

	return snapshot
}

func candidateStrongObservation(
	amt, capacity lnwire.MilliSatoshi) bool {

	if capacity <= 0 {
		return false
	}

	threshold := capacity / 200
	if threshold < 1 {
		threshold = 1
	}

	return amt >= threshold
}

func candidateRecordProbe(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	if edge == nil || amt <= 0 {
		return
	}

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	forward := candidateMutableStateLocked(edge.key)
	if amt > forward.lowerOK {
		forward.lowerOK = amt
	}
	if forward.upperFail != 0 && amt >= forward.upperFail {
		forward.upperFail = 0
	}

	inferred := amt
	strong := candidateStrongObservation(amt, edge.capacity)
	if strong {
		highEstimate := edge.capacity * 97 / 100
		if highEstimate > inferred {
			inferred = highEstimate
		}
		forward.mode = 1
	}

	if !forward.known || inferred > forward.estimate {
		forward.estimate = inferred
	}

	forward.known = true
	forward.confidence = math.Max(forward.confidence, 0.94)
	forward.successes++
	if forward.failures > 0 {
		forward.failures--
	}
	candidateNormalizeState(forward, edge.capacity)

	reverse := candidateMutableStateLocked(
		candidateReverseKey(edge.key),
	)

	reverseUpper := edge.capacity - amt + 1
	if reverseUpper < 1 {
		reverseUpper = 1
	}
	if reverse.upperFail == 0 || reverseUpper < reverse.upperFail {
		reverse.upperFail = reverseUpper
	}
	if reverse.lowerOK >= reverse.upperFail {
		reverse.lowerOK = reverse.upperFail - 1
	}

	if strong {
		reverseEstimate := edge.capacity - forward.estimate
		if reverseEstimate < reverse.lowerOK {
			reverseEstimate = reverse.lowerOK
		}
		if !reverse.known || reverseEstimate < reverse.estimate {
			reverse.estimate = reverseEstimate
		}
		reverse.mode = -1
	}

	reverse.known = true
	reverse.confidence = math.Max(reverse.confidence, 0.86)
	candidateNormalizeState(reverse, edge.capacity)
}

func candidateRecordFailure(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	if edge == nil || amt <= 0 {
		return
	}

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	forward := candidateMutableStateLocked(edge.key)
	if forward.upperFail == 0 || amt < forward.upperFail {
		forward.upperFail = amt
	}
	if forward.lowerOK >= amt {
		forward.lowerOK = amt - 1
	}

	strong := candidateStrongObservation(amt, edge.capacity)
	depletedEstimate := amt / 32
	if strong {
		capFloor := edge.capacity / 1000
		if capFloor < 1 {
			capFloor = 1
		}
		if depletedEstimate > capFloor {
			depletedEstimate = capFloor
		}
		forward.mode = -1
	}

	if depletedEstimate < forward.lowerOK {
		depletedEstimate = forward.lowerOK
	}
	if !forward.known || depletedEstimate < forward.estimate {
		forward.estimate = depletedEstimate
	}

	forward.known = true
	forward.confidence = math.Max(forward.confidence, 0.99)
	forward.failures++
	candidateNormalizeState(forward, edge.capacity)

	reverse := candidateMutableStateLocked(
		candidateReverseKey(edge.key),
	)

	reverseLower := edge.capacity - amt + 1
	if reverseLower < 0 {
		reverseLower = 0
	}
	if reverseLower > reverse.lowerOK {
		reverse.lowerOK = reverseLower
	}
	if reverse.upperFail != 0 && reverse.lowerOK >= reverse.upperFail {
		reverse.upperFail = 0
	}

	reverseEstimate := edge.capacity - forward.estimate
	if reverseEstimate < reverse.lowerOK {
		reverseEstimate = reverse.lowerOK
	}
	if !reverse.known || reverseEstimate > reverse.estimate {
		reverse.estimate = reverseEstimate
	}
	if strong {
		reverse.mode = 1
	}

	reverse.known = true
	reverse.confidence = math.Max(reverse.confidence, 0.97)
	reverse.successes++
	candidateNormalizeState(reverse, edge.capacity)
}

func candidateRecordSettlement(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	if edge == nil || amt <= 0 {
		return
	}

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	forward := candidateMutableStateLocked(edge.key)

	preEstimate := forward.estimate
	if !forward.known || preEstimate < amt {
		preEstimate = amt
		if candidateStrongObservation(amt, edge.capacity) {
			highEstimate := edge.capacity * 97 / 100
			if highEstimate > preEstimate {
				preEstimate = highEstimate
			}
			forward.mode = 1
		}
	}
	if preEstimate > edge.capacity {
		preEstimate = edge.capacity
	}

	forward.estimate = preEstimate - amt
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
	forward.confidence = math.Max(forward.confidence, 0.96)
	forward.successes++
	if forward.failures > 0 {
		forward.failures--
	}
	candidateNormalizeState(forward, edge.capacity)

	reverse := candidateMutableStateLocked(
		candidateReverseKey(edge.key),
	)

	if reverse.lowerOK > edge.capacity-amt {
		reverse.lowerOK = edge.capacity
	} else {
		reverse.lowerOK += amt
	}

	if reverse.upperFail != 0 {
		if reverse.upperFail > edge.capacity-amt {
			reverse.upperFail = 0
		} else {
			reverse.upperFail += amt
		}
	}

	reverse.estimate = edge.capacity - forward.estimate
	if reverse.estimate < reverse.lowerOK {
		reverse.estimate = reverse.lowerOK
	}

	reverse.known = true
	reverse.confidence = math.Max(reverse.confidence, 0.96)
	reverse.successes++
	if reverse.failures > 0 {
		reverse.failures--
	}
	candidateNormalizeState(reverse, edge.capacity)
}

type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edges         map[candidateEdgeKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	sessionPenalty  map[candidateEdgeKey]float64
	sessionBlocked  map[candidateEdgeKey]bool
	sessionFailedAt map[candidateEdgeKey]lnwire.MilliSatoshi
	sessionSuspect  map[candidateEdgeKey]uint32
	routeFailedAt   map[string]lnwire.MilliSatoshi

	attempts       uint32
	failedAttempts uint32
	successfulParts uint32
}

func newCandidateRouter(
	view routing.SimNetworkView,
	source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	if spec == nil {
		return nil, errors.New("payment specification is nil")
	}

	router := &candidateRouter{
		source:           source,
		spec:             spec,
		incomingEdges:    make(map[route.Vertex][]*candidateEdge),
		edges:            make(map[candidateEdgeKey]*candidateEdge),
		localBalances:    make(map[uint64]lnwire.MilliSatoshi),
		sessionPenalty:   make(map[candidateEdgeKey]float64),
		sessionBlocked:   make(map[candidateEdgeKey]bool),
		sessionFailedAt:  make(map[candidateEdgeKey]lnwire.MilliSatoshi),
		sessionSuspect:   make(map[candidateEdgeKey]uint32),
		routeFailedAt:    make(map[string]lnwire.MilliSatoshi),
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

func candidatePriorProbability(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if edge.capacity <= 0 || amt <= 0 || amt > edge.capacity {
		return 0
	}

	ratio := float64(amt) / float64(edge.capacity)

	lowSide := 0.495 * math.Exp(-ratio/0.018)
	highSide := 0.495 /
		(1 + math.Exp((ratio-0.965)/0.018))

	probability := 0.005 + lowSide + highSide
	if probability > 0.999 {
		probability = 0.999
	}
	if probability < 0.0005 {
		probability = 0.0005
	}

	return probability
}

func candidateLowerRetryFactor(
	amt, failedAt lnwire.MilliSatoshi) float64 {

	if failedAt <= 0 {
		return 1
	}
	if amt >= failedAt {
		return 0
	}

	ratio := float64(amt) / float64(failedAt)
	switch {
	case ratio > 0.75:
		return 0.004

	case ratio > 0.40:
		return 0.018

	case ratio > 0.15:
		return 0.075

	case ratio > 0.04:
		return 0.30

	case ratio > 0.01:
		return 0.62

	default:
		return 0.88
	}
}

func candidateLowModeProbability(
	edge *candidateEdge,
	state candidateLiquidityState,
	amt lnwire.MilliSatoshi) float64 {

	if state.lowerOK >= amt {
		return 0.998
	}
	if state.upperFail != 0 && amt >= state.upperFail {
		return 0
	}

	scale := math.Max(float64(edge.capacity)*0.018, 1)
	distance := float64(amt - state.lowerOK)
	if distance < 0 {
		distance = 0
	}

	tail := math.Exp(-distance / scale)
	probability := 0.006 + 0.78*tail

	if state.estimate >= amt {
		probability = math.Max(probability, 0.82)
	}

	if state.upperFail != 0 {
		upperTail := math.Exp(
			-float64(state.upperFail-state.lowerOK) / scale,
		)
		if upperTail < 0.999 {
			tail = (tail - upperTail) / (1 - upperTail)
			if tail < 0 {
				tail = 0
			}
			probability = 0.006 + 0.78*tail
		}
	}

	return probability
}

func (r *candidateRouter) edgeProbability(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if r.sessionBlocked[edge.key] {
		return 0
	}

	retryFactor := candidateLowerRetryFactor(
		amt, r.sessionFailedAt[edge.key],
	)
	if retryFactor == 0 {
		return 0
	}

	if edge.key.from == r.source {
		if r.localBalances[edge.key.chanID] < amt {
			return 0
		}

		return 0.9995
	}

	state := candidateStateSnapshot(edge)
	prior := candidatePriorProbability(edge, amt)
	if prior == 0 {
		return 0
	}

	var probability float64

	switch {
	case state.lowerOK >= amt:
		probability = 0.9985

	case state.upperFail != 0 && amt >= state.upperFail:
		return 0

	case !state.known:
		probability = prior

	case state.mode < 0:
		probability = candidateLowModeProbability(
			edge, state, amt,
		)

	case state.mode > 0 && state.estimate >= amt:
		margin := float64(state.estimate-amt+1) /
			math.Max(float64(edge.capacity), 1)
		probability = 0.975 +
			0.022*state.confidence +
			0.002*math.Min(margin*8, 1)

	case state.upperFail != 0:
		lower := float64(state.lowerOK)
		upper := float64(state.upperFail)
		position := (float64(amt) - lower) /
			math.Max(upper-lower, 1)
		if position < 0 {
			position = 0
		}
		if position > 1 {
			position = 1
		}

		probability = 0.01 +
			0.94*math.Pow(1-position, 2.8)
		probability = 0.90*probability + 0.10*prior

	case state.estimate >= amt:
		margin := float64(state.estimate-amt+1) /
			math.Max(float64(edge.capacity), 1)
		probability = 0.90 +
			0.075*state.confidence +
			0.02*math.Min(margin*5, 1)

	default:
		over := float64(amt-state.estimate) /
			math.Max(float64(edge.capacity), 1)
		probability = prior * 0.12 *
			math.Exp(-over/0.035)
	}

	failedAt := r.sessionFailedAt[edge.key]
	if failedAt != 0 && state.lowerOK < amt {
		probability *= retryFactor
	}

	if penalty := r.sessionPenalty[edge.key]; penalty > 0 {
		probability *= math.Exp(
			-0.70 * math.Min(penalty, 8),
		)
	}

	if probability > 0.999 {
		probability = 0.999
	}
	if probability < 0.000001 {
		probability = 0.000001
	}

	return probability
}

type candidateSearchLabel struct {
	node   route.Vertex
	amount lnwire.MilliSatoshi
	score  float64
	risk   float64
	hops   uint16

	edge  *candidateEdge
	child *candidateSearchLabel

	active bool
}

type candidateSearchQueue []*candidateSearchLabel

func (q candidateSearchQueue) Len() int {
	return len(q)
}

func (q candidateSearchQueue) Less(i, j int) bool {
	return q[i].score < q[j].score
}

func (q candidateSearchQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

func (q *candidateSearchQueue) Push(value any) {
	*q = append(*q, value.(*candidateSearchLabel))
}

func (q *candidateSearchQueue) Pop() any {
	old := *q
	last := len(old) - 1
	item := old[last]
	*q = old[:last]

	return item
}

func candidateLabelContains(
	label *candidateSearchLabel,
	node route.Vertex) bool {

	for current := label; current != nil; current = current.child {
		if current.node == node {
			return true
		}
	}

	return false
}

func candidateLabelRank(
	label *candidateSearchLabel,
	deliver lnwire.MilliSatoshi) float64 {

	amountRatio := float64(label.amount) /
		math.Max(float64(deliver), 1)
	if amountRatio < 1 {
		amountRatio = 1
	}

	return label.score +
		0.10*math.Log(amountRatio) +
		0.014*float64(label.hops)
}

func candidateInsertLabel(
	frontiers map[route.Vertex][]*candidateSearchLabel,
	label *candidateSearchLabel,
	deliver lnwire.MilliSatoshi) bool {

	existing := frontiers[label.node]

	for _, old := range existing {
		if old.active &&
			old.score <= label.score+1e-12 &&
			old.amount <= label.amount &&
			old.hops <= label.hops {

			return false
		}
	}

	kept := make([]*candidateSearchLabel, 0, len(existing)+1)
	for _, old := range existing {
		if !old.active {
			continue
		}

		if label.score <= old.score+1e-12 &&
			label.amount <= old.amount &&
			label.hops <= old.hops {

			old.active = false
			continue
		}

		kept = append(kept, old)
	}

	kept = append(kept, label)
	if len(kept) > candidateMaxLabels {
		worst := 0
		worstRank := candidateLabelRank(kept[0], deliver)

		for i := 1; i < len(kept); i++ {
			rank := candidateLabelRank(kept[i], deliver)
			if rank > worstRank {
				worst = i
				worstRank = rank
			}
		}

		if kept[worst] == label {
			return false
		}

		kept[worst].active = false
		kept = append(kept[:worst], kept[worst+1:]...)
	}

	label.active = true
	frontiers[label.node] = kept

	return true
}

func (r *candidateRouter) routeRejected(
	rt *route.Route,
	deliver lnwire.MilliSatoshi) bool {

	failedAt := r.routeFailedAt[candidateRouteKey(rt)]

	return failedAt != 0 && deliver >= failedAt
}

func (r *candidateRouter) findRoute(
	deliver lnwire.MilliSatoshi) (*route.Route, float64, error) {

	if deliver <= 0 {
		return nil, 0, errors.New("route amount must be positive")
	}
	if r.source == r.spec.Target {
		return nil, 0, errors.New("source is payment target")
	}

	root := &candidateSearchLabel{
		node:   r.spec.Target,
		amount: deliver,
		active: true,
	}

	queue := &candidateSearchQueue{}
	heap.Push(queue, root)

	frontiers := map[route.Vertex][]*candidateSearchLabel{
		r.spec.Target: {root},
	}

	expansions := 0

	for queue.Len() != 0 {
		item := heap.Pop(queue).(*candidateSearchLabel)
		if !item.active {
			continue
		}

		if item.node == r.source {
			built, err := r.buildRoute(deliver, item)
			if err != nil {
				continue
			}
			if r.routeRejected(built, deliver) {
				continue
			}

			return built, item.risk, nil
		}

		if item.hops >= candidateMaxRouteHops {
			continue
		}

		expansions++
		if expansions > candidateSearchLimit {
			break
		}

		for _, edge := range r.incomingEdges[item.node] {
			if candidateLabelContains(item, edge.key.from) {
				continue
			}

			amountOver := item.amount
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
				sending += fee
				if sending < amountOver {
					continue
				}
			}

			edgeRisk := -math.Log(probability)
			feePenalty := 5.0 * float64(fee) /
				math.Max(float64(deliver), 1)

			hopPenalty := 0.045 +
				0.003*float64(item.hops)

			ratio := float64(amountOver) /
				math.Max(float64(edge.capacity), 1)
			capacityPenalty := 0.0
			if ratio > 0.70 {
				x := (ratio - 0.70) / 0.30
				capacityPenalty = 0.30 * x * x
			}

			label := &candidateSearchLabel{
				node:   edge.key.from,
				amount: sending,
				score: item.score + edgeRisk +
					feePenalty + hopPenalty +
					capacityPenalty,
				risk:  item.risk + edgeRisk,
				hops:  item.hops + 1,
				edge:  edge,
				child: item,
			}

			if label.node == r.source {
				label.active = true
				heap.Push(queue, label)
				continue
			}

			if !candidateInsertLabel(
				frontiers, label, deliver,
			) {
				continue
			}

			heap.Push(queue, label)
		}
	}

	return nil, 0, errors.New("no route found")
}

func (r *candidateRouter) buildRoute(
	deliver lnwire.MilliSatoshi,
	sourceLabel *candidateSearchLabel) (*route.Route, error) {

	path := make([]*candidateEdge, 0, sourceLabel.hops)
	current := sourceLabel

	for current != nil && current.edge != nil {
		path = append(path, current.edge)
		current = current.child
	}

	if len(path) == 0 {
		return nil, errors.New("selected route has no hops")
	}
	if current == nil || current.node != r.spec.Target {
		return nil, errors.New("selected route does not reach target")
	}

	amounts := make([]lnwire.MilliSatoshi, len(path))
	expiries := make([]uint32, len(path))

	last := len(path) - 1
	amounts[last] = deliver
	expiries[last] = candidateFinalCltvDelta

	for i := last - 1; i >= 0; i-- {
		nextEdge := path[i+1]
		amounts[i] = amounts[i+1] +
			nextEdge.fee(amounts[i+1])
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

func (r *candidateRouter) candidateShardAmounts(
	amt lnwire.MilliSatoshi,
	partsLeft uint32) []lnwire.MilliSatoshi {

	if partsLeft <= 1 {
		return []lnwire.MilliSatoshi{amt}
	}

	limit := partsLeft
	if limit > 64 {
		limit = 64
	}

	seen := make(map[lnwire.MilliSatoshi]struct{}, limit+32)
	amounts := make([]lnwire.MilliSatoshi, 0, limit+32)

	add := func(shard lnwire.MilliSatoshi) {
		if shard <= 0 || shard > amt {
			return
		}
		if _, ok := seen[shard]; ok {
			return
		}

		seen[shard] = struct{}{}
		amounts = append(amounts, shard)
	}

	minimum := candidateCeilDiv(amt, partsLeft)
	add(amt)
	add(minimum)

	for parts := uint32(2); parts <= limit; parts++ {
		add(candidateCeilDiv(amt, parts))
	}

	for shard := amt / 2; shard >= minimum && shard > 0; shard /= 2 {
		add(shard)
		if shard == minimum {
			break
		}
	}

	for _, failedAt := range r.sessionFailedAt {
		if failedAt <= 1 {
			continue
		}

		for _, divisor := range []lnwire.MilliSatoshi{2, 4, 8, 16, 32} {
			shard := (failedAt - 1) / divisor
			if shard >= minimum {
				add(shard)
			}
		}
	}

	if minimum < amt {
		add(minimum * 2)
		add(minimum * 3)
		add(minimum * 4)
		add(minimum * 6)
		add(minimum * 8)
	}

	return amounts
}

type candidateRouteChoice struct {
	route   *route.Route
	shard   lnwire.MilliSatoshi
	risk    float64
	utility float64
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
	minimum := candidateCeilDiv(amt, partsLeft)
	shards := r.candidateShardAmounts(amt, partsLeft)

	progressWeight := 0.72
	switch {
	case r.successfulParts > 0:
		progressWeight = 0.94

	case r.failedAttempts >= 3:
		progressWeight = 0.50

	case r.failedAttempts > 0:
		progressWeight = 0.60
	}

	var best *candidateRouteChoice

	for _, shard := range shards {
		if shard < minimum {
			continue
		}

		rt, risk, err := r.findRoute(shard)
		if err != nil {
			continue
		}

		progress := math.Log(
			math.Max(
				float64(shard)/float64(minimum),
				1,
			),
		)
		fee := rt.TotalAmount - shard
		feePenalty := 4.0 * float64(fee) /
			math.Max(float64(shard), 1)
		hopPenalty := 0.006 * float64(len(rt.Hops))

		completionBonus := 0.0
		if shard == amt {
			completionBonus = 0.08
		}

		utility := -risk +
			progressWeight*progress +
			completionBonus -
			feePenalty -
			hopPenalty

		choice := &candidateRouteChoice{
			route:   rt,
			shard:   shard,
			risk:    risk,
			utility: utility,
		}

		if best == nil ||
			choice.utility > best.utility+1e-12 ||
			(math.Abs(choice.utility-best.utility) <= 1e-12 &&
				choice.shard > best.shard) {

			best = choice
		}
	}

	if best == nil {
		return nil, errors.New("no route found")
	}

	return best.route, nil
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

func candidateRouteKey(
	rt *route.Route) string {

	key := fmt.Sprintf("%x", rt.SourcePubKey[:])
	for _, hop := range rt.Hops {
		key += fmt.Sprintf(
			"/%d:%x", hop.ChannelID, hop.PubKeyBytes[:],
		)
	}

	return key
}

func (r *candidateRouter) recordSessionFailure(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	previous := r.sessionFailedAt[edge.key]
	if previous == 0 || amt < previous {
		r.sessionFailedAt[edge.key] = amt
	}

	r.sessionPenalty[edge.key] += 1.35
}

func (r *candidateRouter) recordAnonymousFailure(
	rt *route.Route,
	edges []*candidateEdge) {

	routeKey := candidateRouteKey(rt)
	deliver := rt.Hops[len(rt.Hops)-1].AmtToForward

	previous := r.routeFailedAt[routeKey]
	if previous == 0 || deliver < previous {
		r.routeFailedAt[routeKey] = deliver
	}

	type suspect struct {
		edge *candidateEdge
		amt  lnwire.MilliSatoshi
	}

	suspects := make([]suspect, 0, len(edges))
	for i, edge := range edges {
		if edge == nil || edge.key.from == r.source {
			continue
		}

		amount := candidateRouteAmount(rt, i)
		state := candidateStateSnapshot(edge)
		if state.lowerOK >= amount {
			continue
		}

		suspects = append(suspects, suspect{
			edge: edge,
			amt:  amount,
		})
	}

	if len(suspects) == 1 {
		candidateRecordFailure(
			suspects[0].edge, suspects[0].amt,
		)
		r.recordSessionFailure(
			suspects[0].edge, suspects[0].amt,
		)

		return
	}

	if len(suspects) == 0 {
		for _, edge := range edges {
			if edge != nil {
				r.sessionPenalty[edge.key] += 0.35
			}
		}

		return
	}

	share := 2.2 / math.Sqrt(float64(len(suspects)))
	for _, item := range suspects {
		key := item.edge.key
		r.sessionSuspect[key]++
		r.sessionPenalty[key] += share

		if r.sessionSuspect[key] >= 4 {
			r.sessionPenalty[key] += 0.30
		}
		if r.sessionSuspect[key] >= 8 {
			failedAt := r.sessionFailedAt[key]
			if failedAt == 0 || item.amt < failedAt {
				r.sessionFailedAt[key] = item.amt
			}
		}
	}
}

func (r *candidateRouter) shiftSessionLiquidity(
	edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	if failedAt := r.sessionFailedAt[edge.key]; failedAt != 0 {
		if failedAt > amt {
			r.sessionFailedAt[edge.key] = failedAt - amt
		} else {
			r.sessionFailedAt[edge.key] = 1
		}
	}

	reverseKey := candidateReverseKey(edge.key)
	if failedAt := r.sessionFailedAt[reverseKey]; failedAt != 0 {
		if failedAt > edge.capacity-amt {
			delete(r.sessionFailedAt, reverseKey)
		} else {
			r.sessionFailedAt[reverseKey] = failedAt + amt
		}
	}
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
	if len(rt.Hops) == 0 {
		return errors.New("reported route has no hops")
	}

	edges := r.routeEdges(rt)
	routeKey := candidateRouteKey(rt)

	if result.Failure == nil {
		r.successfulParts++

		for i, edge := range edges {
			if edge == nil {
				continue
			}

			amount := candidateRouteAmount(rt, i)
			candidateRecordSettlement(edge, amount)
			r.shiftSessionLiquidity(edge, amount)

			if penalty := r.sessionPenalty[edge.key]; penalty > 0.25 {
				r.sessionPenalty[edge.key] = penalty * 0.12
			} else {
				delete(r.sessionPenalty, edge.key)
			}
			if r.sessionSuspect[edge.key] > 1 {
				r.sessionSuspect[edge.key] /= 2
			} else {
				delete(r.sessionSuspect, edge.key)
			}
		}

		delete(r.routeFailedAt, routeKey)

		firstChan := rt.Hops[0].ChannelID
		spent := rt.TotalAmount
		if r.localBalances[firstChan] > spent {
			r.localBalances[firstChan] -= spent
		} else {
			r.localBalances[firstChan] = 0
		}

		return nil
	}

	r.failedAttempts++

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

			if penalty := r.sessionPenalty[edge.key]; penalty > 0.3 {
				r.sessionPenalty[edge.key] *= 0.25
			} else {
				delete(r.sessionPenalty, edge.key)
			}
			if r.sessionSuspect[edge.key] > 0 {
				r.sessionSuspect[edge.key]--
			}
		}
	}

	code := result.Failure.Code()
	if failIndex >= 0 && failIndex < len(edges) {
		edge := edges[failIndex]
		if edge == nil {
			r.recordAnonymousFailure(rt, edges)
			return nil
		}

		switch code {
		case lnwire.CodeTemporaryChannelFailure:
			amount := candidateRouteAmount(rt, failIndex)
			candidateRecordFailure(edge, amount)
			r.recordSessionFailure(edge, amount)

			if edge.key.from == r.source {
				if r.localBalances[edge.key.chanID] >= amount {
					r.localBalances[edge.key.chanID] = amount - 1
				}
			}

		case lnwire.CodeFeeInsufficient,
			lnwire.CodeIncorrectCltvExpiry:

			r.sessionBlocked[edge.key] = true
			r.sessionPenalty[edge.key] += 6

		default:
			r.sessionBlocked[edge.key] = true
			r.sessionPenalty[edge.key] += 3
		}

		return nil
	}

	if code == lnwire.CodeTemporaryChannelFailure {
		r.recordAnonymousFailure(rt, edges)
		return nil
	}

	previous := r.routeFailedAt[routeKey]
	deliver := rt.Hops[len(rt.Hops)-1].AmtToForward
	if previous == 0 || deliver < previous {
		r.routeFailedAt[routeKey] = deliver
	}

	for _, edge := range edges {
		if edge != nil {
			r.sessionPenalty[edge.key] += 1.0
		}
	}

	return nil
}
// ImportObservations lets mx_c3 accept liquidity observations it did not
// gather itself, which the champion as evolved cannot do.
//
// This is the ONLY change from router_mx3_generalist_v1.go. Nothing in the
// SimRouter contract ever asked a candidate to consume third-party knowledge,
// so no evolved router implements it, and without it the served-weights
// question cannot even be asked of the champions — a sweep would measure
// "imports were never delivered" and read it as "imports did not help".
//
// Every observation is routed through the SAME belief update a real attempt
// would have produced: a success is a probe that proves the edge carried the
// amount, a failure proves it did not. No new evidence is invented and no
// path is special-cased, so the only difference between an imported belief
// and an earned one is that the imported one cost no payment.
//
// NOTE: Part of the routing.SimObservationImporter interface.
func (r *candidateRouter) ImportObservations(
	obs []routing.SimObservation) error {

	for _, o := range obs {
		edge, ok := r.edges[candidateEdgeKey{
			chanID: o.ChanID,
			from:   o.From,
			to:     o.To,
		}]
		if !ok {
			// The server saw a channel this consumer's gossip view
			// does not carry. Silently ignoring it is correct: a
			// served cache describes the network, not this node's
			// subset of it.
			continue
		}

		amt := lnwire.MilliSatoshi(o.AmtMsat)
		if o.Success {
			candidateRecordProbe(edge, amt)

			continue
		}

		candidateRecordFailure(edge, amt)
	}

	return nil
}
