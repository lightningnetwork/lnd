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

func (e *candidateEdge) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return e.baseFeeMsat + amt*e.feeRatePPM/1_000_000
}

func (e *candidateEdge) policyAllows(amt lnwire.MilliSatoshi) bool {
	if amt < e.minHTLC || amt > e.capacity {
		return false
	}
	return e.maxHTLC == 0 || amt <= e.maxHTLC
}

type candidateBelief struct {
	lowerOK   lnwire.MilliSatoshi
	upperFail lnwire.MilliSatoshi
	estimate  lnwire.MilliSatoshi
	successes uint32
	failures  uint32
}

type candidateCurrentFailure struct {
	upper lnwire.MilliSatoshi
	count uint32
}

type candidateNetworkKey struct {
	source      route.Vertex
	fingerprint uint64
}

type candidateNetworkMemory struct {
	beliefs map[candidateEdgeKey]candidateBelief
}

var candidateMemory = struct {
	sync.Mutex
	networks map[candidateNetworkKey]*candidateNetworkMemory
}{
	networks: make(map[candidateNetworkKey]*candidateNetworkMemory),
}

type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	networkKey candidateNetworkKey
	beliefs    map[candidateEdgeKey]candidateBelief

	currentFails  map[candidateEdgeKey]candidateCurrentFailure
	policyBlocked map[candidateEdgeKey]bool
	edgeUses      map[candidateEdgeKey]uint32
	suspect       map[candidateEdgeKey]uint32

	reserved map[candidateEdgeKey]lnwire.MilliSatoshi
	held     []*route.Route
	planned  []*route.Route

	lastFailedShard lnwire.MilliSatoshi
	attempts        int
	attemptLimit    int
}

func candidateMix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func candidateVertexHash(v route.Vertex) uint64 {
	h := uint64(1469598103934665603)
	for i, b := range v {
		h ^= uint64(b) + uint64(i+1)<<8
		h *= 1099511628211
	}
	return h
}

func candidateEdgeHash(e *candidateEdge) uint64 {
	x := candidateMix64(e.key.chanID)
	x ^= candidateMix64(candidateVertexHash(e.key.from))
	x ^= candidateMix64(candidateVertexHash(e.key.to) +
		0x9e3779b97f4a7c15)
	x ^= candidateMix64(uint64(e.capacity))
	x ^= candidateMix64(uint64(e.baseFeeMsat) +
		uint64(e.feeRatePPM)<<17)
	x ^= candidateMix64(uint64(e.timeLockDelta) +
		uint64(e.minHTLC)<<16)
	x ^= candidateMix64(uint64(e.maxHTLC))
	return candidateMix64(x)
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	r := &candidateRouter{
		source:         source,
		spec:           spec,
		incomingEdges:  make(map[route.Vertex][]*candidateEdge),
		localBalances:  localBalances,
		beliefs:        make(map[candidateEdgeKey]candidateBelief),
		currentFails:   make(map[candidateEdgeKey]candidateCurrentFailure),
		policyBlocked:  make(map[candidateEdgeKey]bool),
		edgeUses:       make(map[candidateEdgeKey]uint32),
		suspect:        make(map[candidateEdgeKey]uint32),
		reserved:       make(map[candidateEdgeKey]lnwire.MilliSatoshi),
	}

	maxParts := spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}
	r.attemptLimit = int(maxParts)*3 + 8
	if r.attemptLimit < 24 {
		r.attemptLimit = 24
	}
	if r.attemptLimit > 64 {
		r.attemptLimit = 64
	}

	ctx := context.Background()
	seen := map[route.Vertex]bool{source: true}
	queue := []route.Vertex{source}
	var fingerprint uint64

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		err := view.ForEachNodeDirectedChannel(
			ctx, node, func(ch *graphdb.DirectedChannel) error {
				if !seen[ch.OtherNode] {
					seen[ch.OtherNode] = true
					queue = append(queue, ch.OtherNode)
				}

				pol := ch.InPolicy
				if pol == nil || pol.IsDisabled {
					return nil
				}

				edge := &candidateEdge{
					key: candidateEdgeKey{
						chanID: ch.ChannelID,
						from:   ch.OtherNode,
						to:     node,
					},
					capacity: lnwire.NewMSatFromSatoshis(
						ch.Capacity,
					),
					baseFeeMsat:   pol.FeeBaseMSat,
					feeRatePPM:    pol.FeeProportionalMillionths,
					timeLockDelta: pol.TimeLockDelta,
					minHTLC:       pol.MinHTLC,
				}
				if pol.HasMaxHTLC {
					edge.maxHTLC = pol.MaxHTLC
				}

				r.incomingEdges[node] = append(
					r.incomingEdges[node], edge,
				)
				fingerprint ^= candidateEdgeHash(edge)
				return nil
			}, func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	r.networkKey = candidateNetworkKey{
		source:      source,
		fingerprint: fingerprint,
	}

	candidateMemory.Lock()
	mem := candidateMemory.networks[r.networkKey]
	if mem == nil {
		mem = &candidateNetworkMemory{
			beliefs: make(map[candidateEdgeKey]candidateBelief),
		}
		candidateMemory.networks[r.networkKey] = mem
	}
	for key, belief := range mem.beliefs {
		r.beliefs[key] = belief
	}
	candidateMemory.Unlock()

	return r, nil
}

func candidatePrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity <= 0 || amt > capacity {
		return 0
	}

	x := float64(amt) / float64(capacity)
	lowMode := math.Exp(-x / 0.055)
	highMode := 1 / (1 + math.Exp((x-0.93)/0.035))
	p := 0.5*lowMode + 0.5*highMode

	if p < 0.005 {
		return 0.005
	}
	if p > 0.985 {
		return 0.985
	}
	return p
}

func (r *candidateRouter) probability(edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	reserved := r.reserved[edge.key]
	total := amt + reserved

	if !edge.policyAllows(amt) || total > edge.capacity {
		return 0
	}
	if r.policyBlocked[edge.key] {
		return 0
	}

	retryScale := 1.0
	if failure, ok := r.currentFails[edge.key]; ok {
		if failure.count >= 2 {
			return 0
		}
		if failure.upper > 0 {
			if total >= failure.upper {
				return 0
			}

			retryCeiling := failure.upper * 2 / 3
			if retryCeiling == 0 {
				retryCeiling = 1
			}
			if total > retryCeiling {
				return 0
			}
			retryScale = 0.35
		}
	}

	if edge.key.from == r.source {
		balance, ok := r.localBalances[edge.key.chanID]
		if !ok || total > balance {
			return 0
		}
		return 0.999 * retryScale
	}

	p := candidatePrior(total, edge.capacity)
	belief, ok := r.beliefs[edge.key]
	if !ok {
		return p * retryScale
	}

	if belief.lowerOK > 0 && total <= belief.lowerOK {
		return 0.995 * retryScale
	}

	if belief.upperFail > 0 && total >= belief.upperFail {
		if p > 0.012 {
			p = 0.012
		}
		return p * retryScale
	}

	if belief.lowerOK > 0 && belief.upperFail > belief.lowerOK &&
		total > belief.lowerOK {

		width := float64(belief.upperFail - belief.lowerOK)
		pos := float64(total-belief.lowerOK) / width
		evidence := 0.985 - 0.97*pos
		p = 0.2*p + 0.8*evidence
	} else if belief.upperFail > 0 {
		ratio := float64(total) / float64(belief.upperFail)
		if ratio > 0.55 {
			scale := 1 - 0.92*(ratio-0.55)/0.45
			if scale < 0.08 {
				scale = 0.08
			}
			p *= scale
		}
	} else if belief.lowerOK > 0 && total > belief.lowerOK {
		distance := float64(total-belief.lowerOK) /
			float64(edge.capacity)
		boost := 0.45 * math.Exp(-distance/0.18)
		p += boost * (1 - p)
	}

	if belief.estimate > 0 {
		scale := 0.12 * float64(edge.capacity)
		if scale < 1 {
			scale = 1
		}
		q := 1 / (1 + math.Exp(
			(float64(total)-float64(belief.estimate))/scale,
		))
		p = 0.7*p + 0.3*q
	}

	p *= retryScale
	if p < 0.002 {
		return 0.002
	}
	if p > 0.995 {
		return 0.995
	}
	return p
}

type candidateDijkstraItem struct {
	node     route.Vertex
	score    float64
	required lnwire.MilliSatoshi
	logProb  float64
	idx      int
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
	q[i].idx = i
	q[j].idx = j
}

func (q *candidateDijkstraQueue) Push(x any) {
	item := x.(*candidateDijkstraItem)
	item.idx = len(*q)
	*q = append(*q, item)
}

func (q *candidateDijkstraQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[:n-1]
	return item
}

func (r *candidateRouter) findRoute(amt lnwire.MilliSatoshi) (
	*route.Route, float64, error) {

	if amt <= 0 {
		return nil, 0, errors.New("invalid route amount")
	}
	if r.source == r.spec.Target {
		return nil, 0, errors.New("source equals target")
	}

	const (
		riskWeight = 420_000.0
		hopPenalty = 220.0
	)

	bestScore := make(map[route.Vertex]float64)
	next := make(map[route.Vertex]*candidateEdge)

	bestScore[r.spec.Target] = 0
	pq := &candidateDijkstraQueue{}
	heap.Push(pq, &candidateDijkstraItem{
		node:     r.spec.Target,
		required: amt,
	})

	var sourceLogProb float64

	for pq.Len() > 0 {
		item := heap.Pop(pq).(*candidateDijkstraItem)
		known, ok := bestScore[item.node]
		if !ok || item.score > known+0.0001 {
			continue
		}

		if item.node == r.source {
			sourceLogProb = item.logProb
			break
		}

		for _, edge := range r.incomingEdges[item.node] {
			amtOver := item.required
			p := r.probability(edge, amtOver)
			if p <= 0 {
				continue
			}

			fee := lnwire.MilliSatoshi(0)
			if edge.key.from != r.source {
				fee = edge.fee(amtOver)
			}
			sending := amtOver + fee

			edgeScore := float64(fee) +
				riskWeight*(-math.Log(p)) + hopPenalty

			edgeScore += float64(r.edgeUses[edge.key]) * 22_000
			edgeScore += float64(r.suspect[edge.key]) * 260_000

			if r.reserved[edge.key] > 0 {
				edgeScore += 260_000
			}

			score := item.score + edgeScore
			old, found := bestScore[edge.key.from]
			if found && score >= old {
				continue
			}

			bestScore[edge.key.from] = score
			next[edge.key.from] = edge
			heap.Push(pq, &candidateDijkstraItem{
				node:     edge.key.from,
				score:    score,
				required: sending,
				logProb:  item.logProb + math.Log(p),
			})
		}
	}

	if _, ok := bestScore[r.source]; !ok {
		return nil, 0, errors.New("no route found")
	}

	rt, err := r.buildRoute(amt, next)
	if err != nil {
		return nil, 0, err
	}
	return rt, sourceLogProb, nil
}

func (r *candidateRouter) buildRoute(amt lnwire.MilliSatoshi,
	next map[route.Vertex]*candidateEdge) (*route.Route, error) {

	var path []*candidateEdge
	visited := make(map[route.Vertex]bool)

	for node := r.source; node != r.spec.Target; {
		if visited[node] {
			return nil, errors.New("route contains cycle")
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
		return nil, errors.New("empty route")
	}

	amtOver := make([]lnwire.MilliSatoshi, len(path))
	expiryOver := make([]uint32, len(path))

	last := len(path) - 1
	amtOver[last] = amt
	expiryOver[last] = finalCltvDelta

	for i := last - 1; i >= 0; i-- {
		forwardingEdge := path[i+1]
		amtOver[i] = amtOver[i+1] +
			forwardingEdge.fee(amtOver[i+1])
		expiryOver[i] = expiryOver[i+1] +
			uint32(forwardingEdge.timeLockDelta)
	}

	hops := make([]*route.Hop, len(path))
	for i, edge := range path {
		amtToForward := amt
		outgoingExpiry := uint32(finalCltvDelta)
		if i < last {
			amtToForward = amtOver[i+1]
			outgoingExpiry = expiryOver[i+1]
		}

		hops[i] = &route.Hop{
			PubKeyBytes:      edge.key.to,
			ChannelID:        edge.key.chanID,
			AmtToForward:     amtToForward,
			OutgoingTimeLock: outgoingExpiry,
		}
	}

	return &route.Route{
		TotalTimeLock: expiryOver[0],
		TotalAmount:   amtOver[0],
		SourcePubKey:  r.source,
		Hops:          hops,
	}, nil
}

func candidateRouteAmount(rt *route.Route, channelIndex int) (
	lnwire.MilliSatoshi, bool) {

	if channelIndex < 0 || channelIndex >= len(rt.Hops) {
		return 0, false
	}
	if channelIndex == 0 {
		return rt.TotalAmount, true
	}
	return rt.Hops[channelIndex-1].AmtToForward, true
}

func candidateRouteEdgeKey(rt *route.Route,
	channelIndex int) (candidateEdgeKey, bool) {

	if channelIndex < 0 || channelIndex >= len(rt.Hops) {
		return candidateEdgeKey{}, false
	}

	from := rt.SourcePubKey
	if channelIndex > 0 {
		from = rt.Hops[channelIndex-1].PubKeyBytes
	}
	hop := rt.Hops[channelIndex]

	return candidateEdgeKey{
		chanID: hop.ChannelID,
		from:   from,
		to:     hop.PubKeyBytes,
	}, true
}

func candidateFinalAmount(rt *route.Route) lnwire.MilliSatoshi {
	if rt == nil || len(rt.Hops) == 0 {
		return 0
	}
	return rt.Hops[len(rt.Hops)-1].AmtToForward
}

func candidateCopyReservations(
	src map[candidateEdgeKey]lnwire.MilliSatoshi,
) map[candidateEdgeKey]lnwire.MilliSatoshi {

	dst := make(map[candidateEdgeKey]lnwire.MilliSatoshi, len(src))
	for key, amt := range src {
		dst[key] = amt
	}
	return dst
}

func (r *candidateRouter) reserveRoute(rt *route.Route) {
	for i := range rt.Hops {
		key, ok := candidateRouteEdgeKey(rt, i)
		if !ok {
			continue
		}
		amt, ok := candidateRouteAmount(rt, i)
		if ok {
			r.reserved[key] += amt
		}
	}
}

func (r *candidateRouter) syncReservations(inFlight uint32) {
	r.reserved = make(map[candidateEdgeKey]lnwire.MilliSatoshi)

	if inFlight == 0 {
		r.held = nil
		return
	}

	count := int(inFlight)
	if count > len(r.held) {
		count = len(r.held)
	}
	start := len(r.held) - count

	for _, rt := range r.held[start:] {
		r.reserveRoute(rt)
	}
}

func candidateAddAmount(values *[]lnwire.MilliSatoshi,
	seen map[lnwire.MilliSatoshi]bool, value,
	max lnwire.MilliSatoshi) {

	if value <= 0 {
		value = 1
	}
	if value > max {
		value = max
	}
	if !seen[value] {
		seen[value] = true
		*values = append(*values, value)
	}
}

func (r *candidateRouter) planOnce(total lnwire.MilliSatoshi,
	parts uint32, sizeBias float64) ([]*route.Route, float64, bool) {

	savedReservations := candidateCopyReservations(r.reserved)
	defer func() {
		r.reserved = savedReservations
	}()

	remaining := total
	slots := parts
	var plan []*route.Route
	jointScore := 0.0

	for remaining > 0 && slots > 0 {
		if slots == 1 {
			rt, logProb, err := r.findRoute(remaining)
			if err != nil {
				return nil, 0, false
			}

			fees := rt.TotalAmount - remaining
			jointScore += logProb -
				float64(fees)/4_000_000 - 0.025
			plan = append(plan, rt)
			r.reserveRoute(rt)
			remaining = 0
			break
		}

		base := (remaining + lnwire.MilliSatoshi(slots) - 1) /
			lnwire.MilliSatoshi(slots)

		var sizes []lnwire.MilliSatoshi
		seen := make(map[lnwire.MilliSatoshi]bool)

		candidateAddAmount(&sizes, seen, base, remaining)
		candidateAddAmount(&sizes, seen, base*5/4, remaining)
		candidateAddAmount(&sizes, seen, base*3/2, remaining)
		candidateAddAmount(&sizes, seen, base*2, remaining)
		candidateAddAmount(&sizes, seen, base*3, remaining)
		candidateAddAmount(&sizes, seen, remaining, remaining)

		if r.lastFailedShard > 0 {
			candidateAddAmount(
				&sizes, seen, r.lastFailedShard*5/8,
				remaining,
			)
		}

		var bestRoute *route.Route
		bestSize := lnwire.MilliSatoshi(0)
		bestLogProb := 0.0
		bestUtility := math.Inf(-1)

		for _, size := range sizes {
			rt, logProb, err := r.findRoute(size)
			if err != nil {
				continue
			}

			fees := rt.TotalAmount - size
			sizeReward := math.Log(
				float64(size) / float64(base),
			)
			utility := logProb + sizeBias*sizeReward -
				float64(fees)/4_000_000

			if bestRoute == nil || utility > bestUtility {
				bestRoute = rt
				bestSize = size
				bestLogProb = logProb
				bestUtility = utility
			}
		}

		if bestRoute == nil {
			return nil, 0, false
		}

		fees := bestRoute.TotalAmount - bestSize
		jointScore += bestLogProb -
			float64(fees)/4_000_000 - 0.025

		plan = append(plan, bestRoute)
		r.reserveRoute(bestRoute)
		remaining -= bestSize
		slots--
	}

	if remaining != 0 || len(plan) == 0 {
		return nil, 0, false
	}
	return plan, jointScore, true
}

func (r *candidateRouter) makePlan(total lnwire.MilliSatoshi,
	parts uint32, inFlight uint32) ([]*route.Route, error) {

	if parts <= 1 {
		rt, _, err := r.findRoute(total)
		if err != nil {
			return nil, err
		}
		return []*route.Route{rt}, nil
	}

	if inFlight == 0 && r.lastFailedShard == 0 {
		full, logProb, err := r.findRoute(total)
		if err == nil && logProb >= math.Log(0.22) {
			return []*route.Route{full}, nil
		}
	}

	biases := []float64{0.28, 0.48, 0.72}
	var bestPlan []*route.Route
	bestScore := math.Inf(-1)

	for _, bias := range biases {
		plan, score, ok := r.planOnce(total, parts, bias)
		if !ok {
			continue
		}
		if bestPlan == nil || score > bestScore {
			bestPlan = plan
			bestScore = score
		}
	}

	if bestPlan == nil {
		rt, _, err := r.findRoute(total)
		if err != nil {
			return nil, err
		}
		return []*route.Route{rt}, nil
	}
	return bestPlan, nil
}

func (r *candidateRouter) recordRouteUse(rt *route.Route) {
	for i := range rt.Hops {
		key, ok := candidateRouteEdgeKey(rt, i)
		if ok {
			r.edgeUses[key]++
		}
	}
}

func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt <= 0 {
		return nil, errors.New("payment amount exhausted")
	}
	if r.attempts >= r.attemptLimit {
		return nil, errors.New("routing attempt budget exhausted")
	}

	r.syncReservations(inFlightHtlcs)

	maxParts := r.spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}
	if inFlightHtlcs >= maxParts {
		return nil, errors.New("maximum number of parts in flight")
	}
	partsLeft := maxParts - inFlightHtlcs

	if len(r.planned) > 0 {
		rt := r.planned[0]
		if candidateFinalAmount(rt) > 0 &&
			candidateFinalAmount(rt) <= amt {

			r.planned = r.planned[1:]
			r.recordRouteUse(rt)
			r.attempts++
			return rt, nil
		}
		r.planned = nil
	}

	plan, err := r.makePlan(amt, partsLeft, inFlightHtlcs)
	if err != nil {
		return nil, err
	}

	rt := plan[0]
	if len(plan) > 1 {
		r.planned = append([]*route.Route(nil), plan[1:]...)
	} else {
		r.planned = nil
	}

	r.recordRouteUse(rt)
	r.attempts++
	return rt, nil
}

func (r *candidateRouter) storeBelief(key candidateEdgeKey,
	belief candidateBelief) {

	r.beliefs[key] = belief

	candidateMemory.Lock()
	mem := candidateMemory.networks[r.networkKey]
	if mem == nil {
		mem = &candidateNetworkMemory{
			beliefs: make(map[candidateEdgeKey]candidateBelief),
		}
		candidateMemory.networks[r.networkKey] = mem
	}
	mem.beliefs[key] = belief
	candidateMemory.Unlock()
}

func (r *candidateRouter) learnSuccess(key candidateEdgeKey,
	amt lnwire.MilliSatoshi) {

	if amt <= 0 {
		return
	}

	if failure, ok := r.currentFails[key]; ok &&
		failure.upper > 0 && amt >= failure.upper {

		delete(r.currentFails, key)
	}

	if key.from == r.source {
		return
	}

	belief := r.beliefs[key]
	if amt > belief.lowerOK {
		belief.lowerOK = amt
	}
	if belief.upperFail > 0 && amt >= belief.upperFail {
		belief.upperFail = 0
	}
	if amt > belief.estimate {
		belief.estimate = amt
	}
	belief.successes++
	r.storeBelief(key, belief)
}

func (r *candidateRouter) learnFailure(key candidateEdgeKey,
	amt lnwire.MilliSatoshi) {

	if amt <= 0 {
		return
	}

	current := r.currentFails[key]
	if current.upper == 0 || amt < current.upper {
		current.upper = amt
	}
	current.count++
	r.currentFails[key] = current

	if key.from == r.source {
		return
	}

	belief := r.beliefs[key]
	if belief.upperFail == 0 || amt < belief.upperFail {
		belief.upperFail = amt
	}
	if belief.lowerOK >= amt {
		belief.lowerOK = 0
	}

	estimate := amt * 5 / 8
	if belief.estimate == 0 || belief.estimate >= amt {
		belief.estimate = estimate
	}
	belief.failures++
	r.storeBelief(key, belief)
}

func (r *candidateRouter) markRouteSuspect(rt *route.Route,
	weight uint32) {

	for i := range rt.Hops {
		key, ok := candidateRouteEdgeKey(rt, i)
		if !ok {
			continue
		}
		if key.from == r.source && len(rt.Hops) > 1 {
			continue
		}
		r.suspect[key] += weight
	}
}

func (r *candidateRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	_ = attemptID

	if result.Failure == nil {
		for i := range rt.Hops {
			key, ok := candidateRouteEdgeKey(rt, i)
			if !ok {
				continue
			}
			amt, ok := candidateRouteAmount(rt, i)
			if !ok {
				continue
			}
			r.learnSuccess(key, amt+r.reserved[key])
		}

		r.held = append(r.held, rt)
		r.lastFailedShard = 0
		return nil
	}

	r.planned = nil

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
		r.markRouteSuspect(rt, 2)
		r.lastFailedShard = candidateFinalAmount(rt)
		return nil
	}

	passed := failIdx
	if passed > len(rt.Hops) {
		passed = len(rt.Hops)
	}
	for i := 0; i < passed; i++ {
		key, ok := candidateRouteEdgeKey(rt, i)
		if !ok {
			continue
		}
		amt, ok := candidateRouteAmount(rt, i)
		if ok {
			r.learnSuccess(key, amt+r.reserved[key])
		}
	}

	if failIdx >= len(rt.Hops) {
		r.markRouteSuspect(rt, 1)
		return nil
	}

	key, ok := candidateRouteEdgeKey(rt, failIdx)
	if !ok {
		r.markRouteSuspect(rt, 1)
		return nil
	}
	amtOver, ok := candidateRouteAmount(rt, failIdx)
	if !ok {
		r.markRouteSuspect(rt, 1)
		return nil
	}
	totalRequired := amtOver + r.reserved[key]

	switch result.Failure.Code() {
	case lnwire.CodeTemporaryChannelFailure:
		r.learnFailure(key, totalRequired)
		r.suspect[key]++
		r.lastFailedShard = candidateFinalAmount(rt)

	case lnwire.CodeFeeInsufficient,
		lnwire.CodeIncorrectCltvExpiry:

		r.policyBlocked[key] = true
		r.suspect[key] += 2

	default:
		r.policyBlocked[key] = true
		r.suspect[key] += 2
	}

	return nil
}

// ImportObservations lets atomic1 accept liquidity observations it did not
// gather itself. This is the only change from the exp-010b winner.
//
// Each observation goes through the same learnSuccess / learnFailure path a
// real attempt would take, so an imported belief differs from an earned one
// only in having cost no payment. Note that learnSuccess already declines to
// store beliefs about edges leaving the source: atomic1 evolved its own
// version of the rule exp-012 later measured, that a node should trust its
// own channels over anything it is told about them.
//
// NOTE: Part of the routing.SimObservationImporter interface.
func (r *candidateRouter) ImportObservations(
	obs []routing.SimObservation) error {

	for _, o := range obs {
		key := candidateEdgeKey{
			chanID: o.ChanID,
			from:   o.From,
			to:     o.To,
		}

		amt := lnwire.MilliSatoshi(o.AmtMsat)
		if o.Success {
			r.learnSuccess(key, amt)

			continue
		}

		r.learnFailure(key, amt)
	}

	return nil
}
