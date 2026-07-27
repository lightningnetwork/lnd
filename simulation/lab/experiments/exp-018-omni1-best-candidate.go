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

func (e *candidateEdge) usable(amt lnwire.MilliSatoshi,
	checkMin bool) bool {

	if checkMin && amt < e.minHTLC {
		return false
	}
	if e.maxHTLC != 0 && amt > e.maxHTLC {
		return false
	}
	return amt <= e.capacity
}

type candidateBelief struct {
	lowerOK  lnwire.MilliSatoshi
	upperBad lnwire.MilliSatoshi
	conf     uint8
}

var candidateSharedState = struct {
	sync.Mutex
	beliefs map[candidateEdgeKey]candidateBelief
}{
	beliefs: make(map[candidateEdgeKey]candidateBelief),
}

type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edges         map[candidateEdgeKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	shared  map[candidateEdgeKey]candidateBelief
	current map[candidateEdgeKey]candidateBelief

	reserved   map[candidateEdgeKey]lnwire.MilliSatoshi
	edgePenalty map[candidateEdgeKey]float64
	policyBad  map[candidateEdgeKey]bool

	plan []*route.Route
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
		shared:        make(map[candidateEdgeKey]candidateBelief),
		current:       make(map[candidateEdgeKey]candidateBelief),
		reserved:      make(map[candidateEdgeKey]lnwire.MilliSatoshi),
		edgePenalty:   make(map[candidateEdgeKey]float64),
		policyBad:     make(map[candidateEdgeKey]bool),
	}

	ctx := context.Background()
	seen := map[route.Vertex]bool{source: true}
	queue := []route.Vertex{source}

	for len(queue) != 0 {
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

				key := candidateEdgeKey{
					chanID: ch.ChannelID,
					from:   ch.OtherNode,
					to:     node,
				}
				if _, ok := r.edges[key]; ok {
					return nil
				}

				edge := &candidateEdge{
					key: key,
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

				r.edges[key] = edge
				r.incomingEdges[node] = append(
					r.incomingEdges[node], edge,
				)
				return nil
			}, func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	candidateSharedState.Lock()
	for key := range r.edges {
		if belief, ok := candidateSharedState.beliefs[key]; ok {
			r.shared[key] = belief
		}
	}
	candidateSharedState.Unlock()

	return r, nil
}

func candidatePrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity <= 0 || amt > capacity {
		return 0.005
	}

	ratio := float64(amt) / float64(capacity)
	lowMode := 0.5 * math.Exp(-ratio/0.025)
	highMode := 0.5 / (1 + math.Exp((ratio-0.93)/0.04))
	p := lowMode + highMode

	if p < 0.005 {
		return 0.005
	}
	if p > 0.985 {
		return 0.985
	}
	return p
}

func candidateEvidenceProbability(b candidateBelief,
	amt, capacity lnwire.MilliSatoshi) float64 {

	if b.lowerOK > 0 && amt <= b.lowerOK {
		return 0.995
	}
	if b.upperBad > 0 && amt >= b.upperBad {
		return 0.005
	}

	if b.lowerOK > 0 && b.upperBad > b.lowerOK {
		width := float64(b.upperBad - b.lowerOK)
		pos := float64(amt-b.lowerOK) / width
		p := 0.99 - 0.98*pos
		if p < 0.01 {
			return 0.01
		}
		if p > 0.99 {
			return 0.99
		}
		return p
	}

	if b.upperBad > 0 {
		ratio := float64(amt) / float64(b.upperBad)
		p := 0.02 + 0.90*math.Exp(-ratio/0.12)
		if p > 0.99 {
			return 0.99
		}
		return p
	}

	if b.lowerOK > 0 {
		if capacity <= b.lowerOK {
			return 0.985
		}
		distance := float64(amt-b.lowerOK) / float64(capacity)
		p := 0.58 + 0.40*math.Exp(-distance/0.20)
		if p > 0.99 {
			return 0.99
		}
		return p
	}

	return candidatePrior(amt, capacity)
}

func (r *candidateRouter) edgeProbability(edge *candidateEdge,
	total lnwire.MilliSatoshi) float64 {

	if edge.key.from == r.source {
		if total <= r.localBalances[edge.key.chanID] {
			return 0.999
		}
		return 0.001
	}

	p := candidatePrior(total, edge.capacity)

	if belief, ok := r.shared[edge.key]; ok {
		evidence := candidateEvidenceProbability(
			belief, total, edge.capacity,
		)
		weight := float64(belief.conf) /
			(float64(belief.conf) + 1.5)
		if weight > 0.92 {
			weight = 0.92
		}
		p = p*(1-weight) + evidence*weight
	}

	if belief, ok := r.current[edge.key]; ok {
		evidence := candidateEvidenceProbability(
			belief, total, edge.capacity,
		)
		p = 0.04*p + 0.96*evidence
	}

	if p < 0.005 {
		return 0.005
	}
	if p > 0.995 {
		return 0.995
	}
	return p
}

func candidateUpdateBelief(b candidateBelief,
	amt lnwire.MilliSatoshi, passed bool) candidateBelief {

	if passed {
		if b.upperBad > 0 && amt >= b.upperBad {
			b.upperBad = 0
			b.conf /= 2
		}
		if amt > b.lowerOK {
			b.lowerOK = amt
		}
	} else {
		if b.lowerOK > 0 && amt <= b.lowerOK {
			b.lowerOK = 0
			b.conf /= 2
		}
		if b.upperBad == 0 || amt < b.upperBad {
			b.upperBad = amt
		}
	}

	if b.conf < 16 {
		b.conf++
	}
	return b
}

func (r *candidateRouter) learn(key candidateEdgeKey,
	currentAmt, sharedAmt lnwire.MilliSatoshi, passed bool) {

	r.current[key] = candidateUpdateBelief(
		r.current[key], currentAmt, passed,
	)
	r.shared[key] = candidateUpdateBelief(
		r.shared[key], sharedAmt, passed,
	)

	candidateSharedState.Lock()
	candidateSharedState.beliefs[key] = candidateUpdateBelief(
		candidateSharedState.beliefs[key], sharedAmt, passed,
	)
	candidateSharedState.Unlock()
}

func (r *candidateRouter) hardTotal(edge *candidateEdge) lnwire.MilliSatoshi {
	limit := edge.capacity

	if edge.key.from == r.source {
		local := r.localBalances[edge.key.chanID]
		if local < limit {
			limit = local
		}
	}

	if belief, ok := r.current[edge.key]; ok &&
		belief.upperBad > 0 {

		bound := belief.upperBad - 1
		if bound < 0 {
			bound = 0
		}
		if bound < limit {
			limit = bound
		}
	}

	return limit
}

func candidateRecommendedFromBelief(b candidateBelief,
	capacity lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	var result lnwire.MilliSatoshi

	switch {
	case b.lowerOK > 0 && b.upperBad > b.lowerOK:
		result = b.lowerOK + (b.upperBad-b.lowerOK)/4

	case b.upperBad > 0:
		result = b.upperBad * 58 / 100

	case b.lowerOK > 0:
		result = capacity * 88 / 100
		if b.lowerOK > result {
			result = b.lowerOK
		}

	default:
		result = capacity * 78 / 100
	}

	capLimit := capacity * 98 / 100
	if result > capLimit {
		result = capLimit
	}
	return result
}

func (r *candidateRouter) recommendedTotal(
	edge *candidateEdge) lnwire.MilliSatoshi {

	hard := r.hardTotal(edge)
	if edge.key.from == r.source {
		return hard
	}

	var result lnwire.MilliSatoshi
	if belief, ok := r.current[edge.key]; ok {
		result = candidateRecommendedFromBelief(
			belief, edge.capacity,
		)
	} else if belief, ok := r.shared[edge.key]; ok {
		result = candidateRecommendedFromBelief(
			belief, edge.capacity,
		)
	} else {
		result = edge.capacity * 78 / 100
	}

	if result > hard {
		result = hard
	}
	return result
}

type candidateQueueItem struct {
	node     route.Vertex
	cost     float64
	arriving lnwire.MilliSatoshi
}

type candidateQueue []*candidateQueueItem

func (q candidateQueue) Len() int {
	return len(q)
}

func (q candidateQueue) Less(i, j int) bool {
	return q[i].cost < q[j].cost
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

func (r *candidateRouter) findPath(amt lnwire.MilliSatoshi,
	diversity map[candidateEdgeKey]float64) ([]*candidateEdge, error) {

	if amt <= 0 {
		return nil, errors.New("invalid route amount")
	}
	if r.source == r.spec.Target {
		return nil, errors.New("source equals target")
	}

	dist := map[route.Vertex]float64{r.spec.Target: 0}
	next := make(map[route.Vertex]*candidateEdge)
	pq := &candidateQueue{}
	heap.Push(pq, &candidateQueueItem{
		node:     r.spec.Target,
		arriving: amt,
	})

	for pq.Len() != 0 {
		item := heap.Pop(pq).(*candidateQueueItem)
		best, ok := dist[item.node]
		if !ok || item.cost > best {
			continue
		}
		if item.node == r.source {
			break
		}

		for _, edge := range r.incomingEdges[item.node] {
			if r.policyBad[edge.key] ||
				!edge.usable(item.arriving, true) {

				continue
			}

			total := item.arriving + r.reserved[edge.key]
			if total > r.hardTotal(edge) {
				continue
			}

			probability := r.edgeProbability(edge, total)
			riskCost := -math.Log(probability) * 900_000
			edgeCost := riskCost + 2_000 +
				r.edgePenalty[edge.key] + diversity[edge.key]

			sending := item.arriving
			if edge.key.from != r.source {
				fee := edge.fee(item.arriving)
				sending += fee
				edgeCost += float64(fee)
			}

			newCost := item.cost + edgeCost
			oldCost, exists := dist[edge.key.from]
			if exists && newCost >= oldCost {
				continue
			}

			dist[edge.key.from] = newCost
			next[edge.key.from] = edge
			heap.Push(pq, &candidateQueueItem{
				node:     edge.key.from,
				cost:     newCost,
				arriving: sending,
			})
		}
	}

	if _, ok := next[r.source]; !ok {
		return nil, errors.New("no route found")
	}

	var path []*candidateEdge
	seen := make(map[route.Vertex]bool)
	for node := r.source; node != r.spec.Target; {
		if seen[node] {
			return nil, errors.New("routing cycle")
		}
		seen[node] = true

		edge, ok := next[node]
		if !ok {
			return nil, fmt.Errorf("broken path at %v", node)
		}
		path = append(path, edge)
		node = edge.key.to
	}

	return path, nil
}

func candidateSamePath(a, b []*candidateEdge) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].key != b[i].key {
			return false
		}
	}
	return true
}

func candidatePathAmounts(path []*candidateEdge,
	finalAmt lnwire.MilliSatoshi) []lnwire.MilliSatoshi {

	amounts := make([]lnwire.MilliSatoshi, len(path))
	if len(path) == 0 {
		return amounts
	}

	amounts[len(path)-1] = finalAmt
	for i := len(path) - 2; i >= 0; i-- {
		next := path[i+1]
		amounts[i] = amounts[i+1] + next.fee(amounts[i+1])
	}
	return amounts
}

func (r *candidateRouter) pathWithin(path []*candidateEdge,
	finalAmt lnwire.MilliSatoshi,
	planned map[candidateEdgeKey]lnwire.MilliSatoshi,
	safe, checkMin bool) bool {

	if finalAmt <= 0 || len(path) == 0 {
		return false
	}

	amounts := candidatePathAmounts(path, finalAmt)
	for i, edge := range path {
		if !edge.usable(amounts[i], checkMin) {
			return false
		}

		limit := r.hardTotal(edge)
		if safe {
			limit = r.recommendedTotal(edge)
		}

		total := amounts[i] + r.reserved[edge.key] +
			planned[edge.key]
		if total > limit {
			return false
		}
	}

	return true
}

func (r *candidateRouter) maxPathAmount(path []*candidateEdge,
	want lnwire.MilliSatoshi,
	planned map[candidateEdgeKey]lnwire.MilliSatoshi,
	safe bool) lnwire.MilliSatoshi {

	if want <= 0 {
		return 0
	}

	var low lnwire.MilliSatoshi
	high := want

	for low < high {
		mid := low + (high-low+1)/2
		if r.pathWithin(path, mid, planned, safe, false) {
			low = mid
		} else {
			high = mid - 1
		}
	}

	if low == 0 ||
		!r.pathWithin(path, low, planned, safe, true) {

		return 0
	}
	return low
}

func (r *candidateRouter) pathScore(path []*candidateEdge,
	amt lnwire.MilliSatoshi,
	planned map[candidateEdgeKey]lnwire.MilliSatoshi,
	remaining lnwire.MilliSatoshi) float64 {

	amounts := candidatePathAmounts(path, amt)
	logProbability := 0.0

	for i, edge := range path {
		total := amounts[i] + r.reserved[edge.key] +
			planned[edge.key]
		logProbability += math.Log(r.edgeProbability(edge, total))
	}

	coverage := float64(amt) / float64(remaining)
	if coverage > 1 {
		coverage = 1
	}
	fee := amounts[0] - amt

	return 4*math.Log(coverage) + 0.8*logProbability -
		float64(fee)/5_000_000 - 0.03*float64(len(path))
}

func (r *candidateRouter) buildRoute(path []*candidateEdge,
	amt lnwire.MilliSatoshi) (*route.Route, error) {

	if len(path) == 0 {
		return nil, errors.New("empty path")
	}

	amounts := candidatePathAmounts(path, amt)
	expiries := make([]uint32, len(path))
	expiries[len(path)-1] = finalCltvDelta

	for i := len(path) - 2; i >= 0; i-- {
		expiries[i] = expiries[i+1] +
			uint32(path[i+1].timeLockDelta)
	}

	hops := make([]*route.Hop, len(path))
	for i, edge := range path {
		forwardAmt := amt
		outgoingExpiry := uint32(finalCltvDelta)
		if i+1 < len(path) {
			forwardAmt = amounts[i+1]
			outgoingExpiry = expiries[i+1]
		}

		hops[i] = &route.Hop{
			PubKeyBytes:      edge.key.to,
			ChannelID:        edge.key.chanID,
			AmtToForward:     forwardAmt,
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

func candidateAddPlanned(path []*candidateEdge,
	amt lnwire.MilliSatoshi,
	planned map[candidateEdgeKey]lnwire.MilliSatoshi) {

	amounts := candidatePathAmounts(path, amt)
	for i, edge := range path {
		planned[edge.key] += amounts[i]
	}
}

func (r *candidateRouter) allocatePlan(paths [][]*candidateEdge,
	amt lnwire.MilliSatoshi, parts int,
	safe bool) ([]*route.Route, bool) {

	remaining := amt
	planned := make(map[candidateEdgeKey]lnwire.MilliSatoshi)
	result := make([]*route.Route, 0, parts)

	for len(result) < parts && remaining > 0 {
		bestIdx := -1
		var bestAmount lnwire.MilliSatoshi
		bestScore := math.Inf(-1)

		for i, path := range paths {
			maxAmount := r.maxPathAmount(
				path, remaining, planned, safe,
			)
			if maxAmount == 0 {
				continue
			}

			score := r.pathScore(
				path, maxAmount, planned, remaining,
			)
			if score > bestScore {
				bestIdx = i
				bestAmount = maxAmount
				bestScore = score
			}
		}

		if bestIdx < 0 {
			break
		}

		path := paths[bestIdx]
		rt, err := r.buildRoute(path, bestAmount)
		if err != nil {
			break
		}

		result = append(result, rt)
		candidateAddPlanned(path, bestAmount, planned)
		remaining -= bestAmount
	}

	return result, remaining == 0
}

func candidateAppendProbe(probes []lnwire.MilliSatoshi,
	probe lnwire.MilliSatoshi) []lnwire.MilliSatoshi {

	if probe <= 0 {
		return probes
	}
	for _, existing := range probes {
		if existing == probe {
			return probes
		}
	}
	return append(probes, probe)
}

func (r *candidateRouter) makePlan(amt lnwire.MilliSatoshi,
	parts int) ([]*route.Route, error) {

	if parts < 1 {
		return nil, errors.New("maximum parts reached")
	}

	base := (amt + lnwire.MilliSatoshi(parts) - 1) /
		lnwire.MilliSatoshi(parts)

	var probes []lnwire.MilliSatoshi
	probes = candidateAppendProbe(probes, amt)
	probes = candidateAppendProbe(probes, base)
	probes = candidateAppendProbe(probes, base*3/4)
	probes = candidateAppendProbe(probes, base/2)
	probes = candidateAppendProbe(probes, base/4)
	probes = candidateAppendProbe(probes, base/8)

	rounds := parts + 2
	if rounds > 10 {
		rounds = 10
	}

	var paths [][]*candidateEdge
	for _, probe := range probes {
		diversity := make(map[candidateEdgeKey]float64)

		for i := 0; i < rounds; i++ {
			path, err := r.findPath(probe, diversity)
			if err != nil {
				break
			}

			duplicate := false
			for _, existing := range paths {
				if candidateSamePath(existing, path) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				paths = append(paths, path)
			}

			for _, edge := range path {
				diversity[edge.key] += 2_500_000
			}
		}
	}

	if len(paths) == 0 {
		return nil, errors.New("no route found")
	}

	if plan, ok := r.allocatePlan(paths, amt, parts, true); ok {
		return plan, nil
	}
	if plan, ok := r.allocatePlan(paths, amt, parts, false); ok {
		return plan, nil
	}

	return nil, errors.New("no route set can carry payment")
}

func candidateDelivered(rt *route.Route) lnwire.MilliSatoshi {
	if len(rt.Hops) == 0 {
		return 0
	}
	return rt.Hops[len(rt.Hops)-1].AmtToForward
}

func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	maxParts := r.spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}
	if inFlightHtlcs >= maxParts {
		return nil, errors.New("maximum parts reached")
	}

	if len(r.plan) != 0 {
		next := r.plan[0]
		if candidateDelivered(next) <= amt {
			r.plan = r.plan[1:]
			return next, nil
		}
		r.plan = nil
	}

	partsLeft := int(maxParts - inFlightHtlcs)
	plan, err := r.makePlan(amt, partsLeft)
	if err != nil {
		return nil, err
	}

	r.plan = plan[1:]
	return plan[0], nil
}

func candidateRouteEdge(rt *route.Route,
	index int) candidateEdgeKey {

	from := rt.SourcePubKey
	if index > 0 {
		from = rt.Hops[index-1].PubKeyBytes
	}

	return candidateEdgeKey{
		chanID: rt.Hops[index].ChannelID,
		from:   from,
		to:     rt.Hops[index].PubKeyBytes,
	}
}

func candidateRouteAmount(rt *route.Route,
	index int) lnwire.MilliSatoshi {

	if index == 0 {
		return rt.TotalAmount
	}
	return rt.Hops[index-1].AmtToForward
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

func (r *candidateRouter) learnRoutePassed(rt *route.Route,
	end int, reserve bool) {

	if end > len(rt.Hops) {
		end = len(rt.Hops)
	}

	for i := 0; i < end; i++ {
		key := candidateRouteEdge(rt, i)
		amt := candidateRouteAmount(rt, i)
		total := amt + r.reserved[key]
		r.learn(key, total, amt, true)
	}

	if reserve {
		for i := 0; i < end; i++ {
			key := candidateRouteEdge(rt, i)
			r.reserved[key] += candidateRouteAmount(rt, i)
		}
	}
}

func (r *candidateRouter) ReportAttempt(_ uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if result.Failure == nil {
		r.learnRoutePassed(rt, len(rt.Hops), true)
		return nil
	}

	r.plan = nil
	failIndex := candidateFailureIndex(
		rt, result.FailureSource,
	)

	if failIndex > 0 {
		r.learnRoutePassed(rt, failIndex, false)
	}

	if failIndex < 0 || failIndex >= len(rt.Hops) {
		for i := range rt.Hops {
			key := candidateRouteEdge(rt, i)
			r.edgePenalty[key] += 450_000
		}
		return nil
	}

	key := candidateRouteEdge(rt, failIndex)
	amt := candidateRouteAmount(rt, failIndex)
	total := amt + r.reserved[key]

	switch result.Failure.Code() {
	case lnwire.CodeTemporaryChannelFailure:
		r.learn(key, total, amt, false)
		r.edgePenalty[key] += 700_000

	case lnwire.CodeFeeInsufficient,
		lnwire.CodeIncorrectCltvExpiry:

		r.policyBad[key] = true

	default:
		r.edgePenalty[key] += 1_500_000
	}

	return nil
}