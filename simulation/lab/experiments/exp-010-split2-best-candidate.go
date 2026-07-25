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
	finalCltvDelta   = 40
	maxRouteAttempts = 48
	maxShardChoices  = 36
)

type edgeKey struct {
	chanID   uint64
	from, to route.Vertex
}

type candidateEdge struct {
	key      edgeKey
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

	return e.maxHTLC == 0 || amt <= e.maxHTLC
}

type liquidityBelief struct {
	lowerOK   lnwire.MilliSatoshi
	upperFail lnwire.MilliSatoshi
	estimate  lnwire.MilliSatoshi
	samples   uint32
	failures  uint32
}

var sharedBeliefs = struct {
	sync.Mutex
	values map[edgeKey]liquidityBelief
}{
	values: make(map[edgeKey]liquidityBelief),
}

type attemptKey struct {
	amount lnwire.MilliSatoshi
	hash   uint64
}

type candidateRouter struct {
	source route.Vertex
	spec   *routing.SimPaymentSpec

	incomingEdges map[route.Vertex][]*candidateEdge
	edges         map[edgeKey]*candidateEdge

	localBalances map[uint64]lnwire.MilliSatoshi
	beliefs       map[edgeKey]liquidityBelief

	reserved     map[edgeKey]lnwire.MilliSatoshi
	edgeFailures map[edgeKey]uint32
	broken       map[edgeKey]bool
	attempted    map[attemptKey]uint32

	lastFailedAmt lnwire.MilliSatoshi
	attempts      uint32
}

func newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
	localBalances map[uint64]lnwire.MilliSatoshi,
	spec *routing.SimPaymentSpec) (routing.SimRouter, error) {

	r := &candidateRouter{
		source:        source,
		spec:          spec,
		incomingEdges: make(map[route.Vertex][]*candidateEdge),
		edges:         make(map[edgeKey]*candidateEdge),
		localBalances: make(map[uint64]lnwire.MilliSatoshi),
		beliefs:       make(map[edgeKey]liquidityBelief),
		reserved:      make(map[edgeKey]lnwire.MilliSatoshi),
		edgeFailures:  make(map[edgeKey]uint32),
		broken:        make(map[edgeKey]bool),
		attempted:     make(map[attemptKey]uint32),
	}

	for chanID, balance := range localBalances {
		r.localBalances[chanID] = balance
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
				if _, ok := r.edges[key]; ok {
					return nil
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

	sharedBeliefs.Lock()
	for key, edge := range r.edges {
		belief, ok := sharedBeliefs.values[key]
		if !ok {
			belief.estimate = edge.capacity * 4 / 5
		}
		r.beliefs[key] = belief
	}
	sharedBeliefs.Unlock()

	return r, nil
}

func clampProbability(p float64) float64 {
	switch {
	case p < 0.003:
		return 0.003
	case p > 0.995:
		return 0.995
	default:
		return p
	}
}

func bimodalPrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if capacity <= 0 || amt > capacity {
		return 0.003
	}

	x := float64(amt) / float64(capacity)
	lowMode := 0.50 * math.Exp(-20*x)
	highMode := 0.495 / (1 + math.Exp(24*(x-0.90)))

	return clampProbability(lowMode + highMode)
}

func (r *candidateRouter) edgeProbability(edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	total := amt + r.reserved[edge.key]
	if total > edge.capacity {
		return 0.003
	}

	if edge.key.from == r.source {
		if total <= r.localBalances[edge.key.chanID] {
			return 0.999
		}

		return 0.001
	}

	belief := r.beliefs[edge.key]
	if belief.lowerOK > 0 && total <= belief.lowerOK {
		return 0.995
	}
	if belief.upperFail > 0 && total >= belief.upperFail {
		return 0.003
	}

	prior := bimodalPrior(total, edge.capacity)
	if belief.samples == 0 {
		return prior
	}

	scale := float64(edge.capacity) * 0.08
	if scale < 1 {
		scale = 1
	}
	point := 1 / (1 + math.Exp(
		(float64(total)-float64(belief.estimate))/scale,
	))

	confidence := math.Min(0.82, 0.18*float64(belief.samples))
	probability := (1-confidence)*prior + confidence*point

	// A liquidity failure is evidence for the depleted bimodal state even
	// below the exact failed amount. Smaller probes remain possible, but
	// competing unexplored corridors are preferred.
	if belief.failures > 0 && belief.upperFail > 0 {
		ratio := float64(total) / float64(belief.upperFail)
		if ratio > 1 {
			ratio = 1
		}

		factor := 0.10 + 0.72*math.Exp(-5*ratio)
		factor /= 1 + 0.20*float64(belief.failures-1)
		probability *= factor
	}

	return clampProbability(probability)
}

type dijkstraItem struct {
	node  route.Vertex
	score float64
	amt   lnwire.MilliSatoshi
}

type dijkstraQueue []*dijkstraItem

func (q dijkstraQueue) Len() int {
	return len(q)
}

func (q dijkstraQueue) Less(i, j int) bool {
	return q[i].score < q[j].score
}

func (q dijkstraQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

func (q *dijkstraQueue) Push(value any) {
	*q = append(*q, value.(*dijkstraItem))
}

func (q *dijkstraQueue) Pop() any {
	old := *q
	last := old[len(old)-1]
	*q = old[:len(old)-1]

	return last
}

type routeChoice struct {
	route       *route.Route
	deliver     lnwire.MilliSatoshi
	probability float64
	fee         lnwire.MilliSatoshi
	utility     float64
	keys        []edgeKey
	amounts     []lnwire.MilliSatoshi
}

func (r *candidateRouter) findRoute(
	deliver lnwire.MilliSatoshi) (*routeChoice, error) {

	if deliver <= 0 {
		return nil, errors.New("invalid route amount")
	}
	if r.source == r.spec.Target {
		return nil, errors.New("source is payment target")
	}

	score := map[route.Vertex]float64{r.spec.Target: 0}
	next := make(map[route.Vertex]*candidateEdge)

	pq := &dijkstraQueue{}
	heap.Push(pq, &dijkstraItem{
		node: r.spec.Target,
		amt:  deliver,
	})

	for pq.Len() > 0 {
		item := heap.Pop(pq).(*dijkstraItem)
		best, ok := score[item.node]
		if !ok || item.score > best+1e-12 {
			continue
		}
		if item.node == r.source {
			break
		}

		for _, edge := range r.incomingEdges[item.node] {
			if r.broken[edge.key] || !edge.usable(item.amt) {
				continue
			}

			total := item.amt + r.reserved[edge.key]
			if total > edge.capacity {
				continue
			}
			if edge.key.from == r.source &&
				total > r.localBalances[edge.key.chanID] {

				continue
			}

			probability := r.edgeProbability(edge, item.amt)
			edgeFee := edge.fee(item.amt)
			sending := item.amt + edgeFee
			if edge.key.from == r.source {
				edgeFee = 0
				sending = item.amt
			}

			// Reliability dominates fees. Only the edge proven to fail is
			// penalized, since upstream edges successfully carried a failed
			// attempt far enough to receive the downstream error.
			step := -math.Log(probability) +
				float64(edgeFee)/2_500_000 +
				0.012 +
				0.70*float64(r.edgeFailures[edge.key])
			candidate := item.score + step

			old, exists := score[edge.key.from]
			if exists && candidate >= old {
				continue
			}

			score[edge.key.from] = candidate
			next[edge.key.from] = edge
			heap.Push(pq, &dijkstraItem{
				node:  edge.key.from,
				score: candidate,
				amt:   sending,
			})
		}
	}

	if _, ok := next[r.source]; !ok {
		return nil, errors.New("no route found")
	}

	rt, keys, amounts, err := r.buildRoute(deliver, next)
	if err != nil {
		return nil, err
	}

	probability := 1.0
	for i, key := range keys {
		probability *= r.edgeProbability(r.edges[key], amounts[i])
	}

	return &routeChoice{
		route:       rt,
		deliver:     deliver,
		probability: probability,
		fee:         rt.TotalAmount - deliver,
		keys:        keys,
		amounts:     amounts,
	}, nil
}

func (r *candidateRouter) buildRoute(deliver lnwire.MilliSatoshi,
	next map[route.Vertex]*candidateEdge) (*route.Route, []edgeKey,
	[]lnwire.MilliSatoshi, error) {

	var path []*candidateEdge
	for node := r.source; node != r.spec.Target; {
		edge, ok := next[node]
		if !ok {
			return nil, nil, nil, fmt.Errorf(
				"broken path at %v", node,
			)
		}

		path = append(path, edge)
		node = edge.key.to
		if len(path) > len(r.edges) {
			return nil, nil, nil, errors.New("route contains a cycle")
		}
	}

	if len(path) == 0 {
		return nil, nil, nil, errors.New("empty route")
	}

	amounts := make([]lnwire.MilliSatoshi, len(path))
	expiries := make([]uint32, len(path))
	amounts[len(path)-1] = deliver
	expiries[len(path)-1] = finalCltvDelta

	for i := len(path) - 2; i >= 0; i-- {
		outgoing := path[i+1]
		amounts[i] = amounts[i+1] +
			outgoing.fee(amounts[i+1])
		expiries[i] = expiries[i+1] +
			uint32(outgoing.timeLockDelta)
	}

	hops := make([]*route.Hop, len(path))
	keys := make([]edgeKey, len(path))
	for i, edge := range path {
		forward := deliver
		expiry := uint32(finalCltvDelta)
		if i+1 < len(path) {
			forward = amounts[i+1]
			expiry = expiries[i+1]
		}

		hops[i] = &route.Hop{
			PubKeyBytes:      edge.key.to,
			ChannelID:        edge.key.chanID,
			AmtToForward:     forward,
			OutgoingTimeLock: expiry,
		}
		keys[i] = edge.key
	}

	return &route.Route{
		TotalTimeLock: expiries[0],
		TotalAmount:   amounts[0],
		SourcePubKey:  r.source,
		Hops:          hops,
	}, keys, amounts, nil
}

func addCandidate(values *[]lnwire.MilliSatoshi,
	seen map[lnwire.MilliSatoshi]bool, value, minimum,
	maximum lnwire.MilliSatoshi) {

	if value < minimum {
		value = minimum
	}
	if value > maximum {
		value = maximum
	}
	if value <= 0 || seen[value] {
		return
	}

	seen[value] = true
	*values = append(*values, value)
}

func (r *candidateRouter) candidateAmounts(amt lnwire.MilliSatoshi,
	partsLeft uint32) []lnwire.MilliSatoshi {

	if partsLeft <= 1 {
		return []lnwire.MilliSatoshi{amt}
	}

	minimum := (amt + lnwire.MilliSatoshi(partsLeft) - 1) /
		lnwire.MilliSatoshi(partsLeft)
	if minimum < 1_000 {
		minimum = 1_000
	}

	seen := make(map[lnwire.MilliSatoshi]bool)
	base := make([]lnwire.MilliSatoshi, 0, 16)
	addCandidate(&base, seen, amt, minimum, amt)
	addCandidate(&base, seen, amt*4/5, minimum, amt)
	addCandidate(&base, seen, amt*3/4, minimum, amt)
	addCandidate(&base, seen, amt*2/3, minimum, amt)
	addCandidate(&base, seen, amt/2, minimum, amt)
	addCandidate(&base, seen, amt/3, minimum, amt)
	addCandidate(&base, seen, minimum*2, minimum, amt)
	addCandidate(&base, seen, minimum*3/2, minimum, amt)
	addCandidate(&base, seen, minimum, minimum, amt)

	if r.lastFailedAmt > 0 {
		addCandidate(
			&base, seen, r.lastFailedAmt*3/4, minimum, amt,
		)
		addCandidate(
			&base, seen, r.lastFailedAmt/2, minimum, amt,
		)
		addCandidate(
			&base, seen, r.lastFailedAmt/3, minimum, amt,
		)
	}

	// Known bounds and estimated corridor sizes create unequal split
	// candidates. Complements let one shard leave exactly enough for a
	// differently sized parallel corridor.
	var breakpoints []lnwire.MilliSatoshi
	for key, edge := range r.edges {
		if r.broken[key] {
			continue
		}

		belief := r.beliefs[key]
		candidates := []lnwire.MilliSatoshi{
			edge.capacity * 4 / 5,
			edge.capacity * 2 / 3,
		}
		if belief.lowerOK > 0 {
			candidates = append(candidates, belief.lowerOK)
		}
		if belief.estimate > 0 {
			candidates = append(
				candidates, belief.estimate*4/5,
			)
		}
		if belief.upperFail > 1 {
			candidates = append(
				candidates,
				belief.upperFail*3/4,
				belief.upperFail/2,
			)
		}

		for _, value := range candidates {
			if value >= minimum && value <= amt {
				breakpoints = append(breakpoints, value)
			}

			complement := amt - value
			if complement >= minimum && complement <= amt {
				breakpoints = append(
					breakpoints, complement,
				)
			}
		}
	}

	sort.Slice(breakpoints, func(i, j int) bool {
		return breakpoints[i] < breakpoints[j]
	})

	// Uniformly sample a bounded set of breakpoints so both small and large
	// unequal corridors remain represented.
	limit := maxShardChoices - len(base)
	if limit < 0 {
		limit = 0
	}
	if len(breakpoints) <= limit {
		for _, value := range breakpoints {
			addCandidate(&base, seen, value, minimum, amt)
		}
	} else if limit > 0 {
		for i := 0; i < limit; i++ {
			index := i * (len(breakpoints) - 1)
			if limit > 1 {
				index /= limit - 1
			}
			addCandidate(
				&base, seen, breakpoints[index],
				minimum, amt,
			)
		}
	}

	sort.Slice(base, func(i, j int) bool {
		return base[i] > base[j]
	})

	return base
}

func routeHash(keys []edgeKey) uint64 {
	const (
		offset = uint64(1469598103934665603)
		prime  = uint64(1099511628211)
	)

	hash := offset
	for _, key := range keys {
		hash ^= key.chanID
		hash *= prime
		for _, value := range key.from {
			hash ^= uint64(value)
			hash *= prime
		}
		for _, value := range key.to {
			hash ^= uint64(value)
			hash *= prime
		}
	}

	return hash
}

func (r *candidateRouter) reserveChoice(choice *routeChoice) {
	for i, key := range choice.keys {
		r.reserved[key] += choice.amounts[i]
	}
}

func (r *candidateRouter) releaseChoice(choice *routeChoice) {
	for i, key := range choice.keys {
		reserved := r.reserved[key]
		if reserved <= choice.amounts[i] {
			delete(r.reserved, key)
		} else {
			r.reserved[key] = reserved - choice.amounts[i]
		}
	}
}

func (r *candidateRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	if amt <= 0 {
		return nil, errors.New("payment amount is zero")
	}
	if r.spec.MaxParts == 0 || inFlightHtlcs >= r.spec.MaxParts {
		return nil, errors.New("maximum number of parts reached")
	}
	if r.attempts >= maxRouteAttempts {
		return nil, errors.New("routing attempt budget exhausted")
	}

	partsLeft := r.spec.MaxParts - inFlightHtlcs
	var best *routeChoice

	for _, shard := range r.candidateAmounts(amt, partsLeft) {
		choice, err := r.findRoute(shard)
		if err != nil {
			continue
		}

		shardsNeeded := float64(amt) / float64(shard)
		choice.utility = -math.Log(choice.probability) +
			0.24*math.Log(shardsNeeded) +
			float64(choice.fee)/2_500_000

		key := attemptKey{
			amount: shard,
			hash:   routeHash(choice.keys),
		}
		choice.utility += 3.5 * float64(r.attempted[key])

		// Look one shard ahead while the first route is reserved. This
		// favors unequal route sets whose remaining amount still fits a
		// parallel corridor instead of selecting each shard independently.
		remaining := amt - shard
		if remaining > 0 && partsLeft > 1 {
			r.reserveChoice(choice)

			nextMinimum := (remaining +
				lnwire.MilliSatoshi(partsLeft-1) - 1) /
				lnwire.MilliSatoshi(partsLeft-1)
			nextChoice, nextErr := r.findRoute(nextMinimum)

			r.releaseChoice(choice)

			if nextErr != nil {
				choice.utility += 8
			} else {
				choice.utility += 0.35 *
					-math.Log(nextChoice.probability)
				choice.utility += float64(nextChoice.fee) /
					7_500_000
			}
		}

		if best == nil || choice.utility < best.utility {
			best = choice
		}
	}

	if best == nil {
		return nil, errors.New("no route found")
	}

	r.reserveChoice(best)
	r.attempted[attemptKey{
		amount: best.deliver,
		hash:   routeHash(best.keys),
	}]++
	r.attempts++

	return best.route, nil
}

func routeEdgeData(rt *route.Route) ([]edgeKey,
	[]lnwire.MilliSatoshi) {

	keys := make([]edgeKey, len(rt.Hops))
	amounts := make([]lnwire.MilliSatoshi, len(rt.Hops))
	from := rt.SourcePubKey

	for i, hop := range rt.Hops {
		keys[i] = edgeKey{
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

func (r *candidateRouter) storeBelief(
	key edgeKey, belief liquidityBelief) {

	r.beliefs[key] = belief

	sharedBeliefs.Lock()
	sharedBeliefs.values[key] = belief
	sharedBeliefs.Unlock()
}

func (r *candidateRouter) learnFailure(key edgeKey,
	amt lnwire.MilliSatoshi) {

	belief := r.beliefs[key]
	if belief.upperFail == 0 || amt < belief.upperFail {
		belief.upperFail = amt
	}

	depletedEstimate := amt / 5
	if belief.estimate == 0 || belief.estimate > depletedEstimate {
		belief.estimate = depletedEstimate
	} else {
		belief.estimate =
			(3*belief.estimate + depletedEstimate) / 4
	}

	if belief.lowerOK >= belief.upperFail {
		belief.lowerOK = 0
	}

	belief.samples++
	belief.failures++
	r.storeBelief(key, belief)
}

func (r *candidateRouter) learnProbeSuccess(key edgeKey,
	amt lnwire.MilliSatoshi) {

	edge := r.edges[key]
	if edge == nil {
		return
	}

	belief := r.beliefs[key]
	if amt > belief.lowerOK {
		belief.lowerOK = amt
	}

	optimistic := edge.capacity * 9 / 10
	if belief.estimate < amt {
		belief.estimate = amt
	}
	if belief.estimate < optimistic {
		belief.estimate =
			(belief.estimate + optimistic) / 2
	}

	if belief.upperFail > 0 && belief.upperFail <= belief.lowerOK {
		belief.upperFail = 0
	}
	if belief.failures > 0 {
		belief.failures--
	}

	belief.samples++
	r.storeBelief(key, belief)
}

func subtractFloor(value, amount,
	floor lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	if value <= floor+amount {
		return floor
	}

	return value - amount
}

func maxMSat(a, b lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	if a > b {
		return a
	}

	return b
}

func (r *candidateRouter) learnSettlement(key edgeKey,
	amt lnwire.MilliSatoshi) {

	edge := r.edges[key]
	if edge == nil {
		return
	}

	belief := r.beliefs[key]
	estimate := maxMSat(belief.estimate, amt)
	optimistic := edge.capacity * 9 / 10
	if estimate < optimistic {
		estimate = (estimate + optimistic) / 2
	}

	// A settlement transfers liquidity away from this direction.
	belief.estimate = subtractFloor(estimate, amt, 0)
	belief.lowerOK = subtractFloor(
		maxMSat(belief.lowerOK, amt), amt, 0,
	)
	if belief.upperFail > 0 {
		belief.upperFail = subtractFloor(
			belief.upperFail, amt, 1,
		)
	}
	if belief.failures > 0 {
		belief.failures--
	}
	belief.samples++
	r.storeBelief(key, belief)

	reverse := edgeKey{
		chanID: key.chanID,
		from:   key.to,
		to:     key.from,
	}
	reverseEdge := r.edges[reverse]
	if reverseEdge == nil {
		return
	}

	reverseBelief := r.beliefs[reverse]
	reverseBelief.estimate += amt
	if reverseBelief.estimate > reverseEdge.capacity {
		reverseBelief.estimate = reverseEdge.capacity
	}

	if reverseBelief.lowerOK > 0 {
		reverseBelief.lowerOK += amt
		if reverseBelief.lowerOK > reverseEdge.capacity {
			reverseBelief.lowerOK = reverseEdge.capacity
		}
	}
	if reverseBelief.upperFail > 0 {
		reverseBelief.upperFail += amt
		if reverseBelief.upperFail > reverseEdge.capacity {
			reverseBelief.upperFail = 0
		}
	}
	if reverseBelief.failures > 0 {
		reverseBelief.failures--
	}

	reverseBelief.samples++
	r.storeBelief(reverse, reverseBelief)
}

func (r *candidateRouter) ReportAttempt(_ uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if rt == nil || len(rt.Hops) == 0 {
		return errors.New("attempt route is empty")
	}

	keys, amounts := routeEdgeData(rt)
	for i, key := range keys {
		reserved := r.reserved[key]
		if reserved <= amounts[i] {
			delete(r.reserved, key)
		} else {
			r.reserved[key] = reserved - amounts[i]
		}
	}

	if result.Failure == nil {
		for i, key := range keys {
			r.learnSettlement(key, amounts[i])

			if key.from == r.source {
				balance := r.localBalances[key.chanID]
				if amounts[i] >= balance {
					r.localBalances[key.chanID] = 0
				} else {
					r.localBalances[key.chanID] =
						balance - amounts[i]
				}
			}

			if r.edgeFailures[key] > 0 {
				r.edgeFailures[key]--
			}
		}

		r.lastFailedAmt = 0
		return nil
	}

	failIndex := -1
	if result.FailureSource == rt.SourcePubKey {
		failIndex = 0
	} else {
		for i, hop := range rt.Hops {
			if hop.PubKeyBytes == result.FailureSource {
				failIndex = i + 1
				break
			}
		}
	}

	r.lastFailedAmt = rt.Hops[len(rt.Hops)-1].AmtToForward

	if failIndex < 0 || failIndex >= len(keys) {
		return nil
	}

	// Every edge before the failing outgoing edge successfully forwarded
	// this probe. Record that positive evidence without shifting liquidity,
	// because the failed HTLC is rolled back.
	for i := 0; i < failIndex; i++ {
		r.learnProbeSuccess(keys[i], amounts[i])
		if r.edgeFailures[keys[i]] > 0 {
			r.edgeFailures[keys[i]]--
		}
	}

	key := keys[failIndex]
	failedAmt := amounts[failIndex]
	r.edgeFailures[key]++

	switch result.Failure.Code() {
	case lnwire.CodeTemporaryChannelFailure:
		r.learnFailure(key, failedAmt)

	case lnwire.CodeFeeInsufficient,
		lnwire.CodeIncorrectCltvExpiry:

		r.broken[key] = true
	}

	return nil
}