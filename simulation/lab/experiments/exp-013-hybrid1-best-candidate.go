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

const candidateFinalCltvDelta = 40

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
	if amt <= 0 || amt < e.minHTLC || amt > e.capacity {
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
	edges         map[candidateEdgeKey]*candidateEdge
	localBalances map[uint64]lnwire.MilliSatoshi

	networkKey candidateNetworkKey
	beliefs    map[candidateEdgeKey]candidateBelief

	currentFails  map[candidateEdgeKey]candidateCurrentFailure
	policyBlocked map[candidateEdgeKey]bool
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
	x ^= candidateMix64(
		candidateVertexHash(e.key.to) + 0x9e3779b97f4a7c15,
	)
	x ^= candidateMix64(uint64(e.capacity))
	x ^= candidateMix64(
		uint64(e.baseFeeMsat) + uint64(e.feeRatePPM)<<17,
	)
	x ^= candidateMix64(
		uint64(e.timeLockDelta) + uint64(e.minHTLC)<<16,
	)
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
		edges:          make(map[candidateEdgeKey]*candidateEdge),
		localBalances:  localBalances,
		beliefs:        make(map[candidateEdgeKey]candidateBelief),
		currentFails:   make(map[candidateEdgeKey]candidateCurrentFailure),
		policyBlocked:  make(map[candidateEdgeKey]bool),
		suspect:        make(map[candidateEdgeKey]uint32),
		reserved:       make(map[candidateEdgeKey]lnwire.MilliSatoshi),
	}

	maxParts := spec.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}
	r.attemptLimit = int(maxParts) + 6
	if r.attemptLimit < 10 {
		r.attemptLimit = 10
	}
	if r.attemptLimit > 32 {
		r.attemptLimit = 32
	}

	ctx := context.Background()
	seen := map[route.Vertex]bool{source: true}
	queue := []route.Vertex{source}
	var fingerprint uint64

	for len(queue) != 0 {
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

				edge := &candidateEdge{
					key: candidateEdgeKey{
						chanID: ch.ChannelID,
						from:   ch.OtherNode,
						to:     node,
					},
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
				r.edges[edge.key] = edge
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
	memory := candidateMemory.networks[r.networkKey]
	if memory == nil {
		memory = &candidateNetworkMemory{
			beliefs: make(map[candidateEdgeKey]candidateBelief),
		}
		candidateMemory.networks[r.networkKey] = memory
	}
	for key, belief := range memory.beliefs {
		r.beliefs[key] = belief
	}
	candidateMemory.Unlock()

	return r, nil
}

func candidatePrior(amt, capacity lnwire.MilliSatoshi) float64 {
	if amt <= 0 || capacity <= 0 || amt > capacity {
		return 0
	}

	x := float64(amt) / float64(capacity)
	lowMode := math.Exp(-x / 0.050)
	highMode := 1 / (1 + math.Exp((x-0.925)/0.033))
	p := 0.47*lowMode + 0.53*highMode

	switch {
	case p < 0.005:
		return 0.005
	case p > 0.985:
		return 0.985
	default:
		return p
	}
}

func (r *candidateRouter) liquidityProbability(edge *candidateEdge,
	total lnwire.MilliSatoshi) float64 {

	if total <= 0 || total > edge.capacity ||
		r.policyBlocked[edge.key] {

		return 0
	}

	if edge.key.from == r.source {
		balance, ok := r.localBalances[edge.key.chanID]
		if !ok || total > balance {
			return 0
		}
		return 0.9995
	}

	retryScale := 1.0
	if failure, ok := r.currentFails[edge.key]; ok &&
		failure.upper > 0 {

		if total >= failure.upper {
			return 0
		}

		ratio := float64(total) / float64(failure.upper)
		switch {
		case failure.count >= 3 && ratio > 0.38:
			return 0
		case failure.count >= 2 && ratio > 0.55:
			return 0
		case ratio > 0.78:
			return 0
		case ratio > 0.64:
			retryScale = 0.16
		case ratio > 0.50:
			retryScale = 0.34
		case ratio > 0.36:
			retryScale = 0.58
		default:
			retryScale = 0.82
		}
		if failure.count >= 2 {
			retryScale *= 0.72
		}
	}

	p := candidatePrior(total, edge.capacity)
	belief, ok := r.beliefs[edge.key]
	if !ok {
		return p * retryScale
	}

	switch {
	case belief.lowerOK > 0 && total <= belief.lowerOK:
		p = 0.996

	case belief.upperFail > 0 && total >= belief.upperFail:
		p = 0.003

	case belief.lowerOK > 0 &&
		belief.upperFail > belief.lowerOK:

		width := float64(belief.upperFail - belief.lowerOK)
		position := float64(total-belief.lowerOK) / width
		if position < 0 {
			position = 0
		}
		if position > 1 {
			position = 1
		}

		evidence := 0.995 - 0.991*position
		p = 0.10*p + 0.90*evidence

	case belief.upperFail > 0:
		ratio := float64(total) / float64(belief.upperFail)
		if ratio > 0.20 {
			scale := 1 - 0.90*(ratio-0.20)/0.80
			if scale < 0.08 {
				scale = 0.08
			}
			p *= scale
		}

	case belief.lowerOK > 0:
		distance := float64(total-belief.lowerOK) /
			float64(edge.capacity)
		if distance > 0 {
			boost := 0.62 * math.Exp(-distance/0.18)
			p += boost * (1 - p)
		}
	}

	if belief.estimate > 0 {
		scale := 0.07 * float64(edge.capacity)
		if scale < 1 {
			scale = 1
		}
		estimateP := 1 / (1 + math.Exp(
			(float64(total)-float64(belief.estimate))/scale,
		))

		weight := 0.15
		evidenceCount := belief.successes + belief.failures
		if evidenceCount >= 2 {
			weight = 0.23
		}
		if evidenceCount >= 4 {
			weight = 0.31
		}
		p = (1-weight)*p + weight*estimateP
	}

	p *= retryScale
	switch {
	case p < 0.001:
		return 0.001
	case p > 0.997:
		return 0.997
	default:
		return p
	}
}

func (r *candidateRouter) probability(edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	if !edge.policyAllows(amt) {
		return 0
	}

	reserved := r.reserved[edge.key]
	if reserved > edge.capacity || amt > edge.capacity-reserved {
		return 0
	}

	totalP := r.liquidityProbability(edge, reserved+amt)
	if totalP <= 0 {
		return 0
	}
	if reserved == 0 {
		return totalP
	}

	baseP := r.liquidityProbability(edge, reserved)
	if baseP <= 0 {
		return 0
	}

	p := totalP / baseP
	switch {
	case p < 0.001:
		return 0.001
	case p > 0.997:
		return 0.997
	default:
		return p
	}
}

type candidatePathLabel struct {
	node     route.Vertex
	score    float64
	required lnwire.MilliSatoshi
	logProb  float64
	edge     *candidateEdge
	next     *candidatePathLabel
	hops     int
	active   bool
	index    int
}

type candidatePathQueue []*candidatePathLabel

func (q candidatePathQueue) Len() int {
	return len(q)
}

func (q candidatePathQueue) Less(i, j int) bool {
	return q[i].score < q[j].score
}

func (q candidatePathQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *candidatePathQueue) Push(value any) {
	label := value.(*candidatePathLabel)
	label.index = len(*q)
	*q = append(*q, label)
}

func (q *candidatePathQueue) Pop() any {
	old := *q
	last := len(old) - 1
	label := old[last]
	*q = old[:last]
	return label
}

func candidatePathContains(label *candidatePathLabel,
	node route.Vertex) bool {

	for current := label; current != nil; current = current.next {
		if current.node == node {
			return true
		}
	}
	return false
}

func candidateInsertLabel(labels map[route.Vertex][]*candidatePathLabel,
	label *candidatePathLabel) bool {

	current := labels[label.node]
	for _, old := range current {
		if old.active && old.score <= label.score &&
			old.required <= label.required {

			return false
		}
	}

	kept := current[:0]
	for _, old := range current {
		if old.active && label.score <= old.score &&
			label.required <= old.required {

			old.active = false
			continue
		}
		if old.active {
			kept = append(kept, old)
		}
	}
	kept = append(kept, label)

	if len(kept) > 3 {
		sort.Slice(kept, func(i, j int) bool {
			left := kept[i].score +
				float64(kept[i].required)/16_000_000
			right := kept[j].score +
				float64(kept[j].required)/16_000_000
			return left < right
		})
		for _, old := range kept[3:] {
			old.active = false
		}
		kept = kept[:3]
	}

	labels[label.node] = kept
	return label.active
}

func (r *candidateRouter) findRoute(amt lnwire.MilliSatoshi,
	extraPenalty map[candidateEdgeKey]float64) (*route.Route, float64,
	error) {

	if amt <= 0 {
		return nil, 0, errors.New("invalid route amount")
	}
	if r.source == r.spec.Target {
		return nil, 0, errors.New("source equals target")
	}

	const (
		riskWeight = 780_000.0
		hopPenalty = 22_000.0
		maxHops    = 30
		maxExpand  = 14000
	)

	target := &candidatePathLabel{
		node:     r.spec.Target,
		required: amt,
		active:   true,
	}
	labels := map[route.Vertex][]*candidatePathLabel{
		r.spec.Target: {target},
	}
	queue := &candidatePathQueue{}
	heap.Push(queue, target)

	var sourceLabel *candidatePathLabel
	expanded := 0

	for queue.Len() != 0 && expanded < maxExpand {
		item := heap.Pop(queue).(*candidatePathLabel)
		if !item.active {
			continue
		}
		if item.node == r.source {
			sourceLabel = item
			break
		}
		if item.hops >= maxHops {
			continue
		}

		expanded++
		for _, edge := range r.incomingEdges[item.node] {
			if candidatePathContains(item, edge.key.from) {
				continue
			}

			amountOverEdge := item.required
			p := r.probability(edge, amountOverEdge)
			if p <= 0 {
				continue
			}

			fee := lnwire.MilliSatoshi(0)
			if edge.key.from != r.source {
				fee = edge.fee(amountOverEdge)
			}
			required := amountOverEdge + fee

			edgeScore := float64(fee) +
				riskWeight*(-math.Log(p)) + hopPenalty
			edgeScore += float64(r.suspect[edge.key]) * 310_000
			if extraPenalty != nil {
				edgeScore += extraPenalty[edge.key]
			}

			label := &candidatePathLabel{
				node:     edge.key.from,
				score:    item.score + edgeScore,
				required: required,
				logProb:  item.logProb + math.Log(p),
				edge:     edge,
				next:     item,
				hops:     item.hops + 1,
				active:   true,
			}
			if candidateInsertLabel(labels, label) {
				heap.Push(queue, label)
			}
		}
	}

	if sourceLabel == nil {
		return nil, 0, errors.New("no route found")
	}

	path := make([]*candidateEdge, 0, sourceLabel.hops)
	for current := sourceLabel; current != nil && current.edge != nil;
		current = current.next {

		path = append(path, current.edge)
	}

	rt, err := r.buildRoute(amt, path)
	if err != nil {
		return nil, 0, err
	}
	return rt, sourceLabel.logProb, nil
}

func (r *candidateRouter) buildRoute(amt lnwire.MilliSatoshi,
	path []*candidateEdge) (*route.Route, error) {

	if len(path) == 0 {
		return nil, errors.New("empty route")
	}

	node := r.source
	for _, edge := range path {
		if edge.key.from != node {
			return nil, fmt.Errorf("broken path at %v", node)
		}
		node = edge.key.to
	}
	if node != r.spec.Target {
		return nil, errors.New("path does not reach target")
	}

	amounts := make([]lnwire.MilliSatoshi, len(path))
	expiries := make([]uint32, len(path))
	last := len(path) - 1
	amounts[last] = amt
	expiries[last] = candidateFinalCltvDelta

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

func candidateRouteAmount(rt *route.Route, index int) (
	lnwire.MilliSatoshi, bool) {

	if rt == nil || index < 0 || index >= len(rt.Hops) {
		return 0, false
	}
	if index == 0 {
		return rt.TotalAmount, true
	}
	return rt.Hops[index-1].AmtToForward, true
}

func candidateRouteEdgeKey(rt *route.Route,
	index int) (candidateEdgeKey, bool) {

	if rt == nil || index < 0 || index >= len(rt.Hops) {
		return candidateEdgeKey{}, false
	}

	from := rt.SourcePubKey
	if index > 0 {
		from = rt.Hops[index-1].PubKeyBytes
	}

	return candidateEdgeKey{
		chanID: rt.Hops[index].ChannelID,
		from:   from,
		to:     rt.Hops[index].PubKeyBytes,
	}, true
}

func candidateFinalAmount(rt *route.Route) lnwire.MilliSatoshi {
	if rt == nil || len(rt.Hops) == 0 {
		return 0
	}
	return rt.Hops[len(rt.Hops)-1].AmtToForward
}

func candidateCopyReservations(
	source map[candidateEdgeKey]lnwire.MilliSatoshi,
) map[candidateEdgeKey]lnwire.MilliSatoshi {

	copy := make(map[candidateEdgeKey]lnwire.MilliSatoshi, len(source))
	for key, amount := range source {
		copy[key] = amount
	}
	return copy
}

func candidateReserveInto(
	reservations map[candidateEdgeKey]lnwire.MilliSatoshi,
	rt *route.Route) {

	if rt == nil {
		return
	}
	for i := range rt.Hops {
		key, ok := candidateRouteEdgeKey(rt, i)
		if !ok {
			continue
		}
		amount, ok := candidateRouteAmount(rt, i)
		if ok {
			reservations[key] += amount
		}
	}
}

func (r *candidateRouter) reserveRoute(rt *route.Route) {
	candidateReserveInto(r.reserved, rt)
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
	for _, rt := range r.held[len(r.held)-count:] {
		r.reserveRoute(rt)
	}
}

func candidateAddAmount(values *[]lnwire.MilliSatoshi,
	seen map[lnwire.MilliSatoshi]bool, value, minimum,
	maximum lnwire.MilliSatoshi) {

	if maximum < minimum {
		return
	}
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

func (r *candidateRouter) shardSizes(remaining lnwire.MilliSatoshi,
	slots int) []lnwire.MilliSatoshi {

	if slots <= 1 {
		return []lnwire.MilliSatoshi{remaining}
	}

	minimum := lnwire.MilliSatoshi(1)
	maximum := remaining - lnwire.MilliSatoshi(slots-1)
	if maximum < minimum {
		return nil
	}

	avg := (remaining + lnwire.MilliSatoshi(slots) - 1) /
		lnwire.MilliSatoshi(slots)

	values := make([]lnwire.MilliSatoshi, 0, 12)
	seen := make(map[lnwire.MilliSatoshi]bool)

	for _, value := range []lnwire.MilliSatoshi{
		avg * 2 / 3,
		avg * 5 / 6,
		avg,
		avg * 6 / 5,
		avg * 3 / 2,
	} {
		candidateAddAmount(
			&values, seen, value, minimum, maximum,
		)
	}

	if r.lastFailedShard > 0 {
		for _, value := range []lnwire.MilliSatoshi{
			r.lastFailedShard * 2 / 5,
			r.lastFailedShard / 2,
			r.lastFailedShard * 3 / 5,
		} {
			candidateAddAmount(
				&values, seen, value, minimum, maximum,
			)
		}
	}

	for key, belief := range r.beliefs {
		reserved := r.reserved[key]
		if belief.lowerOK > reserved {
			candidateAddAmount(
				&values, seen,
				(belief.lowerOK-reserved)*19/20,
				minimum, maximum,
			)
		}
		if belief.upperFail > reserved {
			candidateAddAmount(
				&values, seen,
				(belief.upperFail-reserved)*9/20,
				minimum, maximum,
			)
		}
		if belief.estimate > reserved {
			candidateAddAmount(
				&values, seen,
				(belief.estimate-reserved)*4/5,
				minimum, maximum,
			)
		}
	}

	sort.Slice(values, func(i, j int) bool {
		left := math.Abs(math.Log(
			float64(values[i]) / float64(avg),
		))
		right := math.Abs(math.Log(
			float64(values[j]) / float64(avg),
		))
		if left == right {
			return values[i] > values[j]
		}
		return left < right
	})
	if len(values) > 7 {
		values = values[:7]
	}

	return values
}

func candidateSharedPenalty(routes []*route.Route) map[candidateEdgeKey]float64 {
	penalties := make(map[candidateEdgeKey]float64)
	for _, rt := range routes {
		for i := range rt.Hops {
			key, ok := candidateRouteEdgeKey(rt, i)
			if !ok {
				continue
			}
			if key.from == rt.SourcePubKey {
				penalties[key] += 100_000
			} else {
				penalties[key] += 520_000
			}
		}
	}
	return penalties
}

func (r *candidateRouter) planScore(routes []*route.Route,
	reservations, base map[candidateEdgeKey]lnwire.MilliSatoshi) float64 {

	score := 0.0
	for key, total := range reservations {
		old := base[key]
		if total <= old {
			continue
		}

		edge := r.edges[key]
		if edge == nil {
			return math.Inf(-1)
		}

		p := r.liquidityProbability(edge, total)
		if p <= 0 {
			return math.Inf(-1)
		}
		if old > 0 {
			oldP := r.liquidityProbability(edge, old)
			if oldP <= 0 {
				return math.Inf(-1)
			}
			p /= oldP
		}

		if p < 0.001 {
			p = 0.001
		}
		if p > 0.999 {
			p = 0.999
		}
		score += math.Log(p)
	}

	var fees lnwire.MilliSatoshi
	uses := make(map[candidateEdgeKey]uint32)
	for _, rt := range routes {
		finalAmount := candidateFinalAmount(rt)
		if rt.TotalAmount > finalAmount {
			fees += rt.TotalAmount - finalAmount
		}
		for i := range rt.Hops {
			key, ok := candidateRouteEdgeKey(rt, i)
			if ok {
				uses[key]++
			}
		}
	}

	score -= float64(fees) / 30_000_000
	if len(routes) > 1 {
		score -= 0.018 * float64(len(routes)-1)
	}
	for key, count := range uses {
		if count > 1 && key.from != r.source {
			score -= 0.025 * float64(count-1)
		}
	}

	return score
}

type candidatePlanState struct {
	remaining    lnwire.MilliSatoshi
	routes       []*route.Route
	reservations map[candidateEdgeKey]lnwire.MilliSatoshi
	shapePenalty float64
	score        float64
}

func (r *candidateRouter) planWithParts(total lnwire.MilliSatoshi,
	parts int) ([]*route.Route, float64, bool) {

	const beamWidth = 2

	base := candidateCopyReservations(r.reserved)
	saved := candidateCopyReservations(r.reserved)
	defer func() {
		r.reserved = saved
	}()

	targetAverage := float64(total) / float64(parts)
	states := []candidatePlanState{{
		remaining:    total,
		reservations: candidateCopyReservations(base),
	}}

	for depth := 0; depth < parts; depth++ {
		slots := parts - depth
		expanded := make([]candidatePlanState, 0, beamWidth*7)

		for _, state := range states {
			r.reserved = candidateCopyReservations(
				state.reservations,
			)
			sizes := r.shardSizes(state.remaining, slots)
			penalty := candidateSharedPenalty(state.routes)

			for _, size := range sizes {
				if size <= 0 || size > state.remaining {
					continue
				}

				remaining := state.remaining - size
				if slots == 1 && remaining != 0 {
					continue
				}
				if slots > 1 &&
					remaining < lnwire.MilliSatoshi(slots-1) {

					continue
				}

				r.reserved = candidateCopyReservations(
					state.reservations,
				)
				rt, _, err := r.findRoute(size, penalty)
				if err != nil {
					continue
				}

				reservations := candidateCopyReservations(
					state.reservations,
				)
				candidateReserveInto(reservations, rt)

				routes := append(
					append([]*route.Route(nil), state.routes...),
					rt,
				)
				shape := state.shapePenalty
				ratio := float64(size) / targetAverage
				if ratio > 0 {
					shape += 0.10 * math.Abs(math.Log(ratio))
				}

				next := candidatePlanState{
					remaining:    remaining,
					routes:       routes,
					reservations: reservations,
					shapePenalty: shape,
				}
				next.score = r.planScore(
					routes, reservations, base,
				) - shape
				if math.IsInf(next.score, -1) {
					continue
				}
				expanded = append(expanded, next)
			}
		}

		if len(expanded) == 0 {
			return nil, 0, false
		}

		sort.Slice(expanded, func(i, j int) bool {
			return expanded[i].score > expanded[j].score
		})
		if len(expanded) > beamWidth {
			expanded = expanded[:beamWidth]
		}
		states = expanded
	}

	for _, state := range states {
		if state.remaining == 0 {
			score := r.planScore(
				state.routes, state.reservations, base,
			)
			return state.routes, score, true
		}
	}

	return nil, 0, false
}

func candidatePartCounts(maxParts int) []int {
	if maxParts < 2 {
		return nil
	}

	seen := make(map[int]bool)
	counts := make([]int, 0, 4)
	add := func(count int) {
		if count >= 2 && count <= maxParts && !seen[count] {
			seen[count] = true
			counts = append(counts, count)
		}
	}

	add(2)
	add(3)
	add(4)

	largest := maxParts
	if largest > 8 {
		largest = 8
	}
	add(largest)

	sort.Ints(counts)
	return counts
}

func (r *candidateRouter) makePlan(total lnwire.MilliSatoshi,
	parts uint32) ([]*route.Route, error) {

	if parts == 0 {
		return nil, errors.New("no payment parts available")
	}

	fullRoute, fullLogProb, fullErr := r.findRoute(total, nil)
	if parts == 1 {
		if fullErr != nil {
			return nil, fullErr
		}
		return []*route.Route{fullRoute}, nil
	}

	if fullErr == nil && fullLogProb >= math.Log(0.982) &&
		r.lastFailedShard == 0 {

		return []*route.Route{fullRoute}, nil
	}

	base := candidateCopyReservations(r.reserved)
	var best []*route.Route
	bestScore := math.Inf(-1)

	if fullErr == nil {
		reservations := candidateCopyReservations(base)
		candidateReserveInto(reservations, fullRoute)
		best = []*route.Route{fullRoute}
		bestScore = r.planScore(best, reservations, base)
	}

	for _, count := range candidatePartCounts(int(parts)) {
		plan, score, ok := r.planWithParts(total, count)
		if !ok {
			continue
		}
		if best == nil || score > bestScore {
			best = plan
			bestScore = score
		}
	}

	if best == nil {
		if fullErr != nil {
			return nil, fullErr
		}
		return []*route.Route{fullRoute}, nil
	}

	return best, nil
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

	if len(r.planned) != 0 {
		rt := r.planned[0]
		finalAmount := candidateFinalAmount(rt)
		if finalAmount > 0 && finalAmount <= amt {
			r.planned = r.planned[1:]
			r.attempts++
			return rt, nil
		}
		r.planned = nil
	}

	plan, err := r.makePlan(amt, partsLeft)
	if err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return nil, errors.New("empty route plan")
	}

	rt := plan[0]
	if len(plan) > 1 {
		r.planned = append([]*route.Route(nil), plan[1:]...)
	} else {
		r.planned = nil
	}

	r.attempts++
	return rt, nil
}

func (r *candidateRouter) storeBelief(key candidateEdgeKey,
	belief candidateBelief) {

	r.beliefs[key] = belief

	candidateMemory.Lock()
	memory := candidateMemory.networks[r.networkKey]
	if memory == nil {
		memory = &candidateNetworkMemory{
			beliefs: make(map[candidateEdgeKey]candidateBelief),
		}
		candidateMemory.networks[r.networkKey] = memory
	}
	memory.beliefs[key] = belief
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

	estimate := amt
	if edge := r.edges[key]; edge != nil && edge.capacity > 0 {
		ratio := float64(amt) / float64(edge.capacity)
		switch {
		case ratio >= 0.08:
			estimate = edge.capacity * 7 / 8
		case ratio >= 0.035:
			estimate = edge.capacity * 3 / 4
		default:
			estimate = amt * 2
		}
		if estimate > edge.capacity {
			estimate = edge.capacity
		}
	}
	if estimate > belief.estimate {
		belief.estimate = estimate
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

	estimate := amt / 3
	if edge := r.edges[key]; edge != nil {
		lowEstimate := edge.capacity / 18
		if estimate > lowEstimate {
			estimate = lowEstimate
		}
	}
	if estimate <= 0 {
		estimate = 1
	}
	if belief.estimate == 0 || belief.estimate >= amt ||
		estimate < belief.estimate {

		belief.estimate = estimate
	}

	belief.failures++
	r.storeBelief(key, belief)
}

func (r *candidateRouter) markRouteSuspect(rt *route.Route,
	weight uint32) {

	if rt == nil {
		return
	}
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

func (r *candidateRouter) ReportAttempt(_ uint64, rt *route.Route,
	result routing.SimHtlcResult) error {

	if rt == nil {
		return errors.New("nil attempted route")
	}

	if result.Failure == nil {
		for i := range rt.Hops {
			key, ok := candidateRouteEdgeKey(rt, i)
			if !ok {
				continue
			}
			amount, ok := candidateRouteAmount(rt, i)
			if !ok {
				continue
			}

			r.learnSuccess(key, amount+r.reserved[key])
			if r.suspect[key] > 0 {
				r.suspect[key]--
			}
		}

		r.held = append(r.held, rt)
		r.lastFailedShard = 0
		return nil
	}

	r.planned = nil

	failureIndex := -1
	if result.FailureSource == rt.SourcePubKey {
		failureIndex = 0
	} else {
		for i, hop := range rt.Hops {
			if hop.PubKeyBytes == result.FailureSource {
				failureIndex = i + 1
				break
			}
		}
	}

	if failureIndex < 0 {
		r.markRouteSuspect(rt, 2)
		r.lastFailedShard = candidateFinalAmount(rt)
		return nil
	}

	passed := failureIndex
	if passed > len(rt.Hops) {
		passed = len(rt.Hops)
	}
	for i := 0; i < passed; i++ {
		key, ok := candidateRouteEdgeKey(rt, i)
		if !ok {
			continue
		}
		amount, ok := candidateRouteAmount(rt, i)
		if ok {
			r.learnSuccess(key, amount+r.reserved[key])
		}
	}

	if failureIndex >= len(rt.Hops) {
		r.markRouteSuspect(rt, 1)
		r.lastFailedShard = candidateFinalAmount(rt)
		return nil
	}

	key, ok := candidateRouteEdgeKey(rt, failureIndex)
	if !ok {
		r.markRouteSuspect(rt, 1)
		r.lastFailedShard = candidateFinalAmount(rt)
		return nil
	}
	amount, ok := candidateRouteAmount(rt, failureIndex)
	if !ok {
		r.markRouteSuspect(rt, 1)
		r.lastFailedShard = candidateFinalAmount(rt)
		return nil
	}

	totalRequired := amount + r.reserved[key]

	switch result.Failure.Code() {
	case lnwire.CodeTemporaryChannelFailure:
		r.learnFailure(key, totalRequired)
		r.suspect[key]++
		r.lastFailedShard = candidateFinalAmount(rt)

	case lnwire.CodeFeeInsufficient,
		lnwire.CodeIncorrectCltvExpiry:

		r.policyBlocked[key] = true
		r.suspect[key] += 4
		r.lastFailedShard = candidateFinalAmount(rt)

	default:
		r.suspect[key] += 3
		r.lastFailedShard = candidateFinalAmount(rt)
	}

	return nil
}
