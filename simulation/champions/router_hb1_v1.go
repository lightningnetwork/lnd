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

const candidateFinalCltvDelta = 40

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

func (e *candidateEdge) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return e.baseFeeMsat + amt*e.feeRatePPM/1_000_000
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

type candidateLiquidityState struct {
	upperFail lnwire.MilliSatoshi
	lowerOK   lnwire.MilliSatoshi
	estimate  lnwire.MilliSatoshi
	known     bool
	conf      float64
	failures  uint32
	successes uint32
	blocked   bool
}

var candidateKnowledge = struct {
	sync.Mutex
	states map[candidateEdgeKey]*candidateLiquidityState
}{
	states: make(map[candidateEdgeKey]*candidateLiquidityState),
}

func candidateStateSnapshot(
	key candidateEdgeKey) candidateLiquidityState {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	state := candidateKnowledge.states[key]
	if state == nil {
		return candidateLiquidityState{}
	}

	return *state
}

func candidateMutableState(
	key candidateEdgeKey) *candidateLiquidityState {

	state := candidateKnowledge.states[key]
	if state == nil {
		state = &candidateLiquidityState{}
		candidateKnowledge.states[key] = state
	}

	return state
}

func candidateRecordProbe(edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	state := candidateMutableState(edge.key)
	if amt > state.lowerOK {
		state.lowerOK = amt
	}

	highEstimate := edge.capacity * 9 / 10
	if highEstimate < amt {
		highEstimate = amt
	}
	if !state.known || state.estimate < amt {
		state.estimate = highEstimate
	}

	if state.upperFail != 0 && amt >= state.upperFail {
		state.upperFail = 0
	}
	if state.failures > 0 {
		state.failures--
	}

	state.known = true
	state.conf = math.Max(state.conf, 0.85)
	state.successes++
}

func candidateRecordFailure(edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	state := candidateMutableState(edge.key)
	if state.upperFail == 0 || amt < state.upperFail {
		state.upperFail = amt
	}

	if state.lowerOK >= amt {
		state.lowerOK = amt - 1
	}

	depletedEstimate := amt / 8
	if depletedEstimate < state.lowerOK {
		depletedEstimate = state.lowerOK
	}
	if !state.known || state.estimate > depletedEstimate {
		state.estimate = depletedEstimate
	}

	state.known = true
	state.conf = math.Max(state.conf, 0.95)
	state.failures++
}

func candidateBlockEdge(edge *candidateEdge) {
	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	state := candidateMutableState(edge.key)
	state.blocked = true
}

func candidateRecordSettlement(edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	candidateKnowledge.Lock()
	defer candidateKnowledge.Unlock()

	forward := candidateMutableState(edge.key)

	preEstimate := forward.estimate
	if !forward.known {
		preEstimate = edge.capacity * 9 / 10
	}
	if preEstimate < amt {
		preEstimate = amt
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
	forward.conf = math.Max(forward.conf, 0.85)
	forward.successes++
	if forward.failures > 0 {
		forward.failures--
	}

	reverseKey := candidateEdgeKey{
		chanID: edge.key.chanID,
		from:   edge.key.to,
		to:     edge.key.from,
	}
	reverse := candidateMutableState(reverseKey)

	if reverse.known {
		reverse.estimate += amt
		if reverse.estimate > edge.capacity {
			reverse.estimate = edge.capacity
		}
	} else {
		reverse.estimate = amt
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
	reverse.conf = math.Max(reverse.conf, 0.9)
}

type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edges         map[candidateEdgeKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	sessionPenalty map[candidateEdgeKey]float64
	sessionBlocked map[candidateEdgeKey]bool
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

func candidatePriorProbability(edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if edge.capacity <= 0 {
		return 0
	}

	ratio := float64(amt) / float64(edge.capacity)

	lowMode := 0.45 * math.Exp(-ratio/0.025)
	highMode := 0.50 /
		(1 + math.Exp((ratio-0.92)/0.04))

	probability := 0.025 + lowMode + highMode
	if probability > 0.985 {
		probability = 0.985
	}
	if probability < 0.005 {
		probability = 0.005
	}

	return probability
}

func (r *candidateRouter) edgeProbability(edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if r.sessionBlocked[edge.key] {
		return 0
	}

	state := candidateStateSnapshot(edge.key)
	if state.blocked {
		return 0
	}

	if edge.key.from == r.source {
		if r.localBalances[edge.key.chanID] < amt {
			return 0
		}

		return 1
	}

	if state.upperFail != 0 && amt >= state.upperFail {
		return 0
	}
	if state.lowerOK >= amt {
		return 0.995
	}

	prior := candidatePriorProbability(edge, amt)
	if !state.known {
		return prior
	}

	if state.estimate >= amt {
		margin := float64(state.estimate-amt+1) /
			float64(edge.capacity+1)
		probability := 0.78 + 0.17*state.conf +
			0.04*math.Min(margin, 1)

		if probability > 0.995 {
			probability = 0.995
		}

		return probability
	}

	if state.upperFail != 0 {
		relative := float64(amt) /
			float64(state.upperFail)
		if relative > 1 {
			relative = 1
		}

		probability := 0.03 +
			0.35*math.Pow(1-relative, 3) +
			0.15*prior

		if state.failures > state.successes+1 {
			probability *= 0.75
		}
		if probability < 0.01 {
			probability = 0.01
		}

		return probability
	}

	probability := 0.35 * prior
	if state.successes > state.failures {
		probability += 0.15
	}
	if probability > 0.75 {
		probability = 0.75
	}
	if probability < 0.01 {
		probability = 0.01
	}

	return probability
}

type candidateQueueItem struct {
	node   route.Vertex
	amount lnwire.MilliSatoshi
	score  float64
	risk   float64
}

type candidateQueue []*candidateQueueItem

func (q candidateQueue) Len() int {
	return len(q)
}

func (q candidateQueue) Less(i, j int) bool {
	return q[i].score < q[j].score
}

func (q candidateQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

func (q *candidateQueue) Push(value any) {
	*q = append(*q, value.(*candidateQueueItem))
}

func (q *candidateQueue) Pop() any {
	old := *q
	last := len(old) - 1
	item := old[last]
	*q = old[:last]

	return item
}

type candidateRouteChoice struct {
	route   *route.Route
	shard   lnwire.MilliSatoshi
	logRisk float64
}

func (r *candidateRouter) findRoute(
	deliver lnwire.MilliSatoshi) (*route.Route, float64, error) {

	if deliver <= 0 {
		return nil, 0, errors.New("route amount must be positive")
	}
	if r.source == r.spec.Target {
		return nil, 0, errors.New("source is payment target")
	}

	dist := make(map[route.Vertex]float64)
	next := make(map[route.Vertex]*candidateEdge)

	dist[r.spec.Target] = 0
	queue := &candidateQueue{}
	heap.Push(queue, &candidateQueueItem{
		node:   r.spec.Target,
		amount: deliver,
	})

	var sourceRisk float64

	for queue.Len() != 0 {
		item := heap.Pop(queue).(*candidateQueueItem)
		best, ok := dist[item.node]
		if !ok || item.score > best+1e-12 {
			continue
		}

		if item.node == r.source {
			sourceRisk = item.risk
			break
		}

		for _, edge := range r.incomingEdges[item.node] {
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
			}

			logRisk := -math.Log(probability) +
				r.sessionPenalty[edge.key]
			feePenalty := 15 * float64(fee) /
				math.Max(float64(deliver), 1)
			edgeScore := logRisk + feePenalty + 0.012
			newScore := item.score + edgeScore

			oldScore, exists := dist[edge.key.from]
			if exists && newScore >= oldScore {
				continue
			}

			dist[edge.key.from] = newScore
			next[edge.key.from] = edge
			heap.Push(queue, &candidateQueueItem{
				node:   edge.key.from,
				amount: sending,
				score:  newScore,
				risk:   item.risk + logRisk,
			})
		}
	}

	if _, ok := dist[r.source]; !ok {
		return nil, 0, errors.New("no route found")
	}

	built, err := r.buildRoute(deliver, next)
	if err != nil {
		return nil, 0, err
	}

	return built, sourceRisk, nil
}

func (r *candidateRouter) buildRoute(deliver lnwire.MilliSatoshi,
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

func candidateCeilDiv(amt lnwire.MilliSatoshi,
	divisor uint32) lnwire.MilliSatoshi {

	if divisor <= 1 {
		return amt
	}

	d := lnwire.MilliSatoshi(divisor)
	return amt/d + lnwire.MilliSatoshi(boolToInt(amt%d != 0))
}

func boolToInt(value bool) int64 {
	if value {
		return 1
	}

	return 0
}

func candidateShardAmounts(amt lnwire.MilliSatoshi,
	partsLeft uint32) []lnwire.MilliSatoshi {

	if partsLeft <= 1 {
		return []lnwire.MilliSatoshi{amt}
	}

	limit := partsLeft
	if limit > 24 {
		limit = 24
	}

	amounts := make([]lnwire.MilliSatoshi, 0, limit+1)
	var previous lnwire.MilliSatoshi

	for parts := uint32(1); parts <= limit; parts++ {
		shard := candidateCeilDiv(amt, parts)
		if shard != previous {
			amounts = append(amounts, shard)
			previous = shard
		}
	}

	minimum := candidateCeilDiv(amt, partsLeft)
	if minimum != previous {
		amounts = append(amounts, minimum)
	}

	return amounts
}

func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt <= 0 {
		return nil, errors.New("remaining amount must be positive")
	}
	if r.attempts >= 96 {
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
	minimum := shards[len(shards)-1]

	threshold := 0.20
	if partsLeft <= 2 {
		threshold = 0.08
	}

	var fallback *candidateRouteChoice
	bestUtility := math.Inf(-1)

	for _, shard := range shards {
		rt, logRisk, err := r.findRoute(shard)
		if err != nil {
			continue
		}

		probability := math.Exp(-logRisk)
		if probability >= threshold {
			return rt, nil
		}

		progress := math.Log(
			math.Max(float64(shard)/float64(minimum), 1),
		)
		fee := rt.TotalAmount - shard
		feePenalty := 10 * float64(fee) /
			math.Max(float64(shard), 1)
		utility := -logRisk + 0.22*progress - feePenalty

		if fallback == nil || utility > bestUtility {
			fallback = &candidateRouteChoice{
				route:   rt,
				shard:   shard,
				logRisk: logRisk,
			}
			bestUtility = utility
		}
	}

	if fallback == nil {
		return nil, errors.New("no route found")
	}

	return fallback.route, nil
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

func candidateRouteAmount(rt *route.Route,
	channelIndex int) lnwire.MilliSatoshi {

	if channelIndex == 0 {
		return rt.TotalAmount
	}

	return rt.Hops[channelIndex-1].AmtToForward
}

func candidateFailureIndex(rt *route.Route,
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

func (r *candidateRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	_ = attemptID
	r.attempts++

	if rt == nil {
		return errors.New("reported route is nil")
	}

	edges := r.routeEdges(rt)

	if result.Failure == nil {
		for i, edge := range edges {
			if edge == nil {
				continue
			}

			amount := candidateRouteAmount(rt, i)
			candidateRecordSettlement(edge, amount)
			delete(r.sessionPenalty, edge.key)
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

		case lnwire.CodeFeeInsufficient,
			lnwire.CodeIncorrectCltvExpiry:

			candidateBlockEdge(edge)

		default:
			r.sessionBlocked[edge.key] = true
		}

		return nil
	}

	for _, edge := range edges {
		if edge == nil || edge.key.from == r.source {
			continue
		}

		r.sessionPenalty[edge.key] += 0.45
	}

	return nil
}