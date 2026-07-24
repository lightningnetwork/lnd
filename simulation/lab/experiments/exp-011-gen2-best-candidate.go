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
	finalCltvDelta      = 40
	lowerRetryPercent   = 45
	maxFailuresPerLevel = 3
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

type candidateBelief struct {
	lowerOK  lnwire.MilliSatoshi
	upperFail lnwire.MilliSatoshi
	estimate lnwire.MilliSatoshi
	evidence uint32
}

var candidateSharedKnowledge = struct {
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
	localSpent    map[uint64]lnwire.MilliSatoshi
	localReserved map[uint64]lnwire.MilliSatoshi
	pendingRoutes map[*route.Route]bool

	beliefs  map[candidateEdgeKey]*candidateBelief
	policyBad map[candidateEdgeKey]bool
	penalties map[candidateEdgeKey]float64

	shardAmt       lnwire.MilliSatoshi
	failuresAtSize uint32
	forceReduce    bool
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	if spec == nil {
		return nil, errors.New("payment specification is nil")
	}

	r := &candidateRouter{
		source:        source,
		spec:          spec,
		incomingEdges: make(map[route.Vertex][]*candidateEdge),
		edges:         make(map[candidateEdgeKey]*candidateEdge),
		localBalances: localBalances,
		localSpent:    make(map[uint64]lnwire.MilliSatoshi),
		localReserved: make(map[uint64]lnwire.MilliSatoshi),
		pendingRoutes: make(map[*route.Route]bool),
		beliefs:       make(map[candidateEdgeKey]*candidateBelief),
		policyBad:     make(map[candidateEdgeKey]bool),
		penalties:     make(map[candidateEdgeKey]float64),
		shardAmt:      spec.Amount,
	}

	ctx := context.Background()
	seenNodes := map[route.Vertex]bool{source: true}
	seenEdges := make(map[candidateEdgeKey]bool)
	queue := []route.Vertex{source}

	for len(queue) != 0 {
		node := queue[0]
		queue = queue[1:]

		err := view.ForEachNodeDirectedChannel(
			ctx, node, func(ch *graphdb.DirectedChannel) error {
				if !seenNodes[ch.OtherNode] {
					seenNodes[ch.OtherNode] = true
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
				if seenEdges[key] {
					return nil
				}
				seenEdges[key] = true

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

				r.edges[key] = edge
				r.incomingEdges[key.to] = append(
					r.incomingEdges[key.to], edge,
				)

				return nil
			}, func() {},
		)
		if err != nil {
			return nil, err
		}
	}

	return r, nil
}

func clampProbability(p float64, low float64, high float64) float64 {
	if p < low {
		return low
	}
	if p > high {
		return high
	}

	return p
}

func bimodalPrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity <= 0 || amt > capacity {
		return 0.005
	}

	x := float64(amt) / float64(capacity)

	// The low mode represents channels depleted in this direction. The
	// high mode represents channels whose funds are almost entirely usable.
	lowMode := math.Exp(-x / 0.08)

	z := (x - 0.90) / 0.035
	var highMode float64
	switch {
	case z >= 60:
		highMode = 0
	case z <= -60:
		highMode = 1
	default:
		highMode = 1 / (1 + math.Exp(z))
	}

	return clampProbability(
		0.5*lowMode+0.5*highMode, 0.005, 0.985,
	)
}

func (r *candidateRouter) belief(edge *candidateEdge) *candidateBelief {
	if belief, ok := r.beliefs[edge.key]; ok {
		return belief
	}

	candidateSharedKnowledge.Lock()
	saved := candidateSharedKnowledge.beliefs[edge.key]
	candidateSharedKnowledge.Unlock()

	belief := saved
	r.beliefs[edge.key] = &belief

	return &belief
}

func (r *candidateRouter) persistBelief(key candidateEdgeKey,
	belief *candidateBelief) {

	candidateSharedKnowledge.Lock()
	candidateSharedKnowledge.beliefs[key] = *belief
	candidateSharedKnowledge.Unlock()
}

func (r *candidateRouter) probability(edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	belief := r.belief(edge)

	if belief.lowerOK > 0 && amt <= belief.lowerOK {
		return 0.995
	}
	if belief.upperFail > 0 && amt >= belief.upperFail {
		return 0.001
	}

	prior := bimodalPrior(amt, edge.capacity)
	if belief.estimate <= 0 || belief.evidence == 0 {
		return prior
	}

	scale := math.Max(float64(edge.capacity)*0.05, 1)
	z := (float64(amt) - float64(belief.estimate)) / scale

	var pointProbability float64
	switch {
	case z >= 60:
		pointProbability = 0
	case z <= -60:
		pointProbability = 1
	default:
		pointProbability = 1 / (1 + math.Exp(z))
	}

	confidence := float64(belief.evidence) /
		float64(belief.evidence+2)
	probability := prior*(1-confidence) +
		pointProbability*confidence

	if belief.lowerOK > 0 && belief.upperFail > belief.lowerOK {
		width := float64(belief.upperFail - belief.lowerOK)
		position := float64(amt-belief.lowerOK) / width
		position = clampProbability(position, 0, 1)

		boundProbability := 0.995*(1-position) + 0.001*position
		probability = 0.4*probability + 0.6*boundProbability
	}

	return clampProbability(probability, 0.001, 0.995)
}

func inferredRichLiquidity(amt,
	capacity lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	if amt >= capacity {
		return capacity
	}

	return amt + (capacity-amt)*3/4
}

func incrementEvidence(belief *candidateBelief) {
	if belief.evidence < 100 {
		belief.evidence++
	}
}

func (r *candidateRouter) observePass(edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	belief := r.belief(edge)

	if amt > belief.lowerOK {
		belief.lowerOK = amt
	}
	if belief.upperFail > 0 && amt >= belief.upperFail {
		belief.upperFail = 0
	}

	inferred := inferredRichLiquidity(amt, edge.capacity)
	if inferred > belief.estimate {
		belief.estimate = inferred
	}

	incrementEvidence(belief)
	r.persistBelief(edge.key, belief)
}

func (r *candidateRouter) observeFailure(edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	belief := r.belief(edge)

	if belief.upperFail == 0 || amt < belief.upperFail {
		belief.upperFail = amt
	}
	if belief.lowerOK >= amt {
		if amt > 1 {
			belief.lowerOK = amt - 1
		} else {
			belief.lowerOK = 0
		}
	}

	failedEstimate := amt * lowerRetryPercent / 100
	if belief.estimate == 0 || failedEstimate < belief.estimate {
		belief.estimate = failedEstimate
	}

	incrementEvidence(belief)
	r.persistBelief(edge.key, belief)
}

func (r *candidateRouter) observeSettlement(edge *candidateEdge,
	amt lnwire.MilliSatoshi) {

	belief := r.belief(edge)
	inferred := inferredRichLiquidity(amt, edge.capacity)

	if inferred > belief.estimate {
		belief.estimate = inferred
	}

	proven := belief.lowerOK
	if amt > proven {
		proven = amt
	}
	if proven > amt {
		belief.lowerOK = proven - amt
	} else {
		belief.lowerOK = 0
	}

	if belief.estimate > amt {
		belief.estimate -= amt
	} else {
		belief.estimate = 0
	}

	if belief.upperFail > amt {
		belief.upperFail -= amt
	} else {
		belief.upperFail = 0
	}

	incrementEvidence(belief)
	r.persistBelief(edge.key, belief)

	reverseKey := candidateEdgeKey{
		chanID: edge.key.chanID,
		from:   edge.key.to,
		to:     edge.key.from,
	}
	reverse, ok := r.edges[reverseKey]
	if !ok {
		return
	}

	reverseBelief := r.belief(reverse)
	if reverseBelief.lowerOK+amt > reverse.capacity {
		reverseBelief.lowerOK = reverse.capacity
	} else {
		reverseBelief.lowerOK += amt
	}

	if reverseBelief.estimate+amt > reverse.capacity {
		reverseBelief.estimate = reverse.capacity
	} else {
		reverseBelief.estimate += amt
	}

	if reverseBelief.upperFail > 0 {
		if reverseBelief.upperFail+amt > reverse.capacity {
			reverseBelief.upperFail = 0
		} else {
			reverseBelief.upperFail += amt
		}
	}

	incrementEvidence(reverseBelief)
	r.persistBelief(reverseKey, reverseBelief)
}

type candidateQueueItem struct {
	node     route.Vertex
	score    float64
	arriving lnwire.MilliSatoshi
	index    int
}

type candidateQueue []*candidateQueueItem

func (q candidateQueue) Len() int {
	return len(q)
}

func (q candidateQueue) Less(i, j int) bool {
	if q[i].score == q[j].score {
		return q[i].arriving < q[j].arriving
	}

	return q[i].score < q[j].score
}

func (q candidateQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *candidateQueue) Push(value any) {
	item := value.(*candidateQueueItem)
	item.index = len(*q)
	*q = append(*q, item)
}

func (q *candidateQueue) Pop() any {
	old := *q
	last := len(old) - 1
	item := old[last]
	old[last] = nil
	*q = old[:last]

	return item
}

func (r *candidateRouter) localAvailable(chanID uint64) lnwire.MilliSatoshi {
	balance := r.localBalances[chanID]
	used := r.localSpent[chanID] + r.localReserved[chanID]
	if used >= balance {
		return 0
	}

	return balance - used
}

func (r *candidateRouter) findRoute(
	amt lnwire.MilliSatoshi) (*route.Route, error) {

	bestScore := make(map[route.Vertex]float64)
	bestAmount := make(map[route.Vertex]lnwire.MilliSatoshi)
	next := make(map[route.Vertex]*candidateEdge)
	settled := make(map[route.Vertex]bool)

	bestScore[r.spec.Target] = 0
	bestAmount[r.spec.Target] = amt

	queue := &candidateQueue{}
	heap.Push(queue, &candidateQueueItem{
		node:     r.spec.Target,
		score:    0,
		arriving: amt,
	})

	for queue.Len() != 0 {
		item := heap.Pop(queue).(*candidateQueueItem)
		if settled[item.node] {
			continue
		}

		score, ok := bestScore[item.node]
		if !ok || item.score > score {
			continue
		}

		settled[item.node] = true
		if item.node == r.source {
			break
		}

		for _, edge := range r.incomingEdges[item.node] {
			if settled[edge.key.from] || r.policyBad[edge.key] {
				continue
			}

			amtOver := item.arriving
			if !edge.usable(amtOver) {
				continue
			}

			belief := r.belief(edge)
			if belief.upperFail > 0 && amtOver >= belief.upperFail {
				continue
			}

			if edge.key.from == r.source &&
				r.localAvailable(edge.key.chanID) < amtOver {

				continue
			}

			sending := amtOver
			feeCost := float64(0)
			probability := float64(1)

			if edge.key.from != r.source {
				fee := edge.fee(amtOver)
				sending += fee
				feeCost = float64(fee)
				probability = r.probability(edge, amtOver)
			}

			// Reliability dominates fees, while the amount-scaled risk
			// naturally favors larger channels and shorter paths.
			riskCost := -math.Log(probability) *
				float64(amtOver) * 0.30
			historyCost := r.penalties[edge.key] *
				float64(amtOver) * 0.25
			newScore := item.score + feeCost + riskCost + historyCost

			oldScore, exists := bestScore[edge.key.from]
			oldAmount := bestAmount[edge.key.from]
			if exists && newScore >= oldScore && sending >= oldAmount {
				continue
			}

			if !exists || newScore < oldScore {
				bestScore[edge.key.from] = newScore
				bestAmount[edge.key.from] = sending
				next[edge.key.from] = edge
				heap.Push(queue, &candidateQueueItem{
					node:     edge.key.from,
					score:    newScore,
					arriving: sending,
				})
			}
		}
	}

	if !settled[r.source] {
		return nil, errors.New("no route found")
	}

	return r.buildRoute(amt, next)
}

func (r *candidateRouter) buildRoute(amt lnwire.MilliSatoshi,
	next map[route.Vertex]*candidateEdge) (*route.Route, error) {

	var path []*candidateEdge
	seen := make(map[route.Vertex]bool)

	for node := r.source; node != r.spec.Target; {
		if seen[node] {
			return nil, errors.New("cycle in selected route")
		}
		seen[node] = true

		edge, ok := next[node]
		if !ok {
			return nil, fmt.Errorf("broken path at %v", node)
		}

		path = append(path, edge)
		node = edge.key.to
	}

	if len(path) == 0 {
		return nil, errors.New("source and target are identical")
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

func paymentPartsLeft(maxParts, inFlight uint32) uint32 {
	if maxParts == 0 {
		maxParts = 1
	}
	if inFlight >= maxParts {
		return 0
	}

	return maxParts - inFlight
}

func minimumShard(remaining lnwire.MilliSatoshi,
	partsLeft uint32) lnwire.MilliSatoshi {

	if partsLeft <= 1 {
		return remaining
	}

	divisor := lnwire.MilliSatoshi(partsLeft)
	return (remaining + divisor - 1) / divisor
}

func reducedShard(current, remaining lnwire.MilliSatoshi,
	partsLeft uint32) lnwire.MilliSatoshi {

	next := current * lowerRetryPercent / 100
	minimum := minimumShard(remaining, partsLeft)

	if next < minimum {
		next = minimum
	}
	if next > remaining {
		next = remaining
	}

	return next
}

func (r *candidateRouter) reserveRoute(rt *route.Route) {
	if len(rt.Hops) == 0 {
		return
	}

	chanID := rt.Hops[0].ChannelID
	r.localReserved[chanID] += rt.TotalAmount
	r.pendingRoutes[rt] = true
}

func (r *candidateRouter) releaseRoute(rt *route.Route, settled bool) {
	if len(rt.Hops) == 0 || !r.pendingRoutes[rt] {
		return
	}
	delete(r.pendingRoutes, rt)

	chanID := rt.Hops[0].ChannelID
	reserved := r.localReserved[chanID]
	if reserved > rt.TotalAmount {
		r.localReserved[chanID] = reserved - rt.TotalAmount
	} else {
		r.localReserved[chanID] = 0
	}

	if settled {
		r.localSpent[chanID] += rt.TotalAmount
	}
}

func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt <= 0 {
		return nil, errors.New("payment amount is zero")
	}

	partsLeft := paymentPartsLeft(r.spec.MaxParts, inFlightHtlcs)
	if partsLeft == 0 {
		return nil, errors.New("maximum in-flight parts reached")
	}

	if r.shardAmt <= 0 || r.shardAmt > amt {
		r.shardAmt = amt
	}

	minimum := minimumShard(amt, partsLeft)
	if r.shardAmt < minimum {
		r.shardAmt = minimum
	}

	if r.forceReduce {
		next := reducedShard(r.shardAmt, amt, partsLeft)
		if next < r.shardAmt {
			r.shardAmt = next
		}
		r.forceReduce = false
		r.failuresAtSize = 0
	}

	for {
		rt, err := r.findRoute(r.shardAmt)
		if err == nil {
			r.reserveRoute(rt)
			return rt, nil
		}

		if partsLeft <= 1 {
			return nil, err
		}

		next := reducedShard(r.shardAmt, amt, partsLeft)
		if next >= r.shardAmt {
			return nil, err
		}

		r.shardAmt = next
		r.failuresAtSize = 0
	}
}

func routeEdgeAmount(rt *route.Route,
	index int) lnwire.MilliSatoshi {

	if index == 0 {
		return rt.TotalAmount
	}

	return rt.Hops[index-1].AmtToForward
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

func failingEdgeIndex(rt *route.Route,
	source route.Vertex) int {

	if source == rt.SourcePubKey {
		return 0
	}

	for i, hop := range rt.Hops {
		if hop.PubKeyBytes != source {
			continue
		}

		index := i + 1
		if index < len(rt.Hops) {
			return index
		}

		return -1
	}

	return -1
}

func (r *candidateRouter) weakestUnknownEdge(edges []*candidateEdge,
	rt *route.Route) int {

	weakest := -1
	weakestProbability := float64(2)

	for i, edge := range edges {
		if edge == nil || edge.key.from == r.source {
			continue
		}

		probability := r.probability(
			edge, routeEdgeAmount(rt, i),
		)
		if probability < weakestProbability {
			weakest = i
			weakestProbability = probability
		}
	}

	if weakest >= 0 {
		return weakest
	}

	for i, edge := range edges {
		if edge != nil {
			return i
		}
	}

	return -1
}

func (r *candidateRouter) penalizeRoute(edges []*candidateEdge) {
	for _, edge := range edges {
		if edge == nil || edge.key.from == r.source {
			continue
		}

		r.penalties[edge.key]++
	}
}

func (r *candidateRouter) ReportAttempt(_ uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if rt == nil {
		return errors.New("attempt route is nil")
	}

	edges := r.routeEdges(rt)

	if result.Failure == nil {
		r.releaseRoute(rt, true)

		for i, edge := range edges {
			if edge == nil {
				continue
			}

			r.observeSettlement(
				edge, routeEdgeAmount(rt, i),
			)
			if r.penalties[edge.key] > 0 {
				r.penalties[edge.key] *= 0.5
			}
		}

		r.failuresAtSize = 0
		return nil
	}

	r.releaseRoute(rt, false)
	r.failuresAtSize++

	failIndex := failingEdgeIndex(rt, result.FailureSource)
	if failIndex >= 0 {
		for i := 0; i < failIndex && i < len(edges); i++ {
			if edges[i] != nil {
				r.observePass(
					edges[i], routeEdgeAmount(rt, i),
				)
			}
		}
	}

	code := result.Failure.Code()
	switch code {
	case lnwire.CodeTemporaryChannelFailure:
		if failIndex < 0 || failIndex >= len(edges) ||
			edges[failIndex] == nil {

			failIndex = r.weakestUnknownEdge(edges, rt)
		}

		if failIndex >= 0 && failIndex < len(edges) &&
			edges[failIndex] != nil {

			edge := edges[failIndex]
			r.observeFailure(
				edge, routeEdgeAmount(rt, failIndex),
			)
			r.penalties[edge.key]++
		} else {
			r.penalizeRoute(edges)
		}

	case lnwire.CodeFeeInsufficient,
		lnwire.CodeIncorrectCltvExpiry:

		if failIndex >= 0 && failIndex < len(edges) &&
			edges[failIndex] != nil {

			r.policyBad[edges[failIndex].key] = true
		} else {
			r.penalizeRoute(edges)
		}

	default:
		r.penalizeRoute(edges)
	}

	if r.failuresAtSize >= maxFailuresPerLevel {
		r.forceReduce = true
	}

	return nil
}