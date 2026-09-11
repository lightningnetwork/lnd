package routing

import (
	"container/heap"
	"context"
	"math"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// The weights below turn a candidate hop into a cost. The stock path finder
// minimizes fee plus a time lock penalty, divided through by the route
// probability; this one minimizes an additive score whose dominant term is the
// negative log of the hop probability. Additivity is what makes a label setting
// search over the graph tractable, and it is far gentler on a low probability
// route than dividing by a probability is.
const (
	// intervalFeeWeight sets the fee sensitivity of the search when the
	// payment carries no fee budget. See intervalFeePenalty for what this
	// number means and for the units it is expressed in, which is the part
	// of this cost function most worth understanding.
	intervalFeeWeight = 5.0

	// intervalHopBase and intervalHopGrowth price adding another hop. The
	// penalty grows with depth, so a long route becomes progressively more
	// expensive rather than paying a flat toll per hop.
	intervalHopBase   = 0.045
	intervalHopGrowth = 0.003

	// intervalCapacityKnee is the channel utilization at which the capacity
	// penalty starts, and intervalCapacityWeight is its weight at full
	// utilization. This steers the search away from channels the payment
	// would nearly fill even when nothing is known against them.
	intervalCapacityKnee   = 0.70
	intervalCapacityWeight = 0.30

	// intervalLabelAmountWeight and intervalLabelHopWeight rank labels
	// against each other when a node holds more of them than it is allowed
	// to keep. The worst by this rank is the one evicted.
	intervalLabelAmountWeight = 0.10
	intervalLabelHopWeight    = 0.014

	// intervalBudgetShare is the fraction of a payment's remaining fee
	// budget it will spend to buy one nat of reliability. Half means a
	// payment will pay up to half of what it has left to raise the
	// probability of a route by a factor of e, which leaves the other half
	// for the hops that follow.
	intervalBudgetShare = 2.0

	// intervalMinFeePrice and intervalMaxFeePrice bound the budget derived
	// exchange rate, in millisatoshis per nat. The floor keeps a payment
	// with almost nothing left from refusing to pay any fee at all, since a
	// route it can afford is still better than no route. The ceiling keeps a
	// payment with a very large budget from treating fees as free.
	intervalMinFeePrice = 30_000.0
	intervalMaxFeePrice = 420_000.0
)

// intervalFeeRate says how a payment converts a fee into the nats its search
// score is denominated in. It has two fields on purpose, because the two
// questions it answers have different answers and confusing them is a bug we
// have already made once.
//
// Whether the payment has a budget at all is latched from the limit the payment
// was created with, and never changes for as long as the payment lives. How
// dearly a payment with a budget prices reliability comes from what that limit
// has left, which shrinks as shards commit and which the lifecycle recomputes
// before every route request.
//
// Inferring the first from the second is what goes wrong. lnd hands a session
// the remaining budget, so a payment with no limit carries the sentinel only
// until its first shard pays a fee; from the second shard on it carries the
// sentinel minus that fee, which is not the sentinel. Read as a classification
// that says budgeted, and every unbudgeted payment that splits silently starts
// pricing fees against a budget nobody set.
type intervalFeeRate struct {
	// budgeted is latched from the payment's own fee limit and decides which
	// branch of penalty applies.
	budgeted bool

	// price is how many millisatoshis of fee buy one nat of log
	// probability, derived from what the budget has left. It is only
	// meaningful when budgeted is set.
	price float64
}

// newIntervalFeeRate builds the rate for one route request. The caller passes
// the latched classification and the budget remaining right now, which is the
// only combination that keeps the two apart.
func newIntervalFeeRate(budgeted bool,
	remaining lnwire.MilliSatoshi) intervalFeeRate {

	if !budgeted {
		return intervalFeeRate{}
	}

	return intervalFeeRate{
		budgeted: true,
		price:    intervalBudgetPrice(remaining),
	}
}

// penalty converts a fee into nats. A payment with a budget pays the rate its
// budget sets; one without pays a fixed fraction of the amount it is sending.
//
// The unbudgeted branch is written as the one expression it has always been,
// weight times fee over the amount, rather than as a division by the reciprocal
// of that. The two agree in exact arithmetic and they do not agree in floating
// point: over the amounts a real corpus holds they differ by one unit in the
// last place about a quarter of the time, because dividing the amount first
// rounds once and dividing the fee by that result rounds again.
//
// One unit in the last place is normally beneath notice. It is not beneath
// notice here, because the frontier compares these scores exactly, both to
// decide whether one label dominates another and to decide which label to
// evict when a node is full. A tie that used to break one way breaks the other,
// a different route comes back, and the payment goes somewhere else. When that
// change was made by accident it was worth 0.032 of objective on one tier, all
// of it in success. So the rule for this branch is bit identity with what came
// before, not algebraic identity, and the way to keep that is to leave the
// expression alone.
func (r intervalFeeRate) penalty(fee float64, amt lnwire.MilliSatoshi,
	weight float64) float64 {

	if r.budgeted {
		return fee / r.price
	}

	return weight * fee / math.Max(float64(amt), 1)
}

// intervalBudgetPrice returns how many millisatoshis of fee a payment will
// trade for one nat of log probability, given what its budget has left. It is
// the exchange rate between the two things the search is minimizing, and it is
// where the units of the cost function are decided.
//
// The score this rate feeds is denominated in nats: a hop contributes the
// negative log of its probability, plus its fee converted through this rate.
// Which way that conversion runs turns out to decide whether a fee budget can
// ever influence the search at all.
//
// The routers this design came from converted fees at a rate proportional to
// the amount being sent, k times fee over amount with k around 5. Read as a
// price, that is a willingness to pay amount/k for one nat, which is a fifth
// of the payment. No realistic fee budget is anywhere near a fifth of it,
// so the fee term never binds and the search prices reliability as though money
// were free. Measurement bore that out: those routers walk into fee budgets
// they cannot see, while lnd, whose path finding prices fees in absolute
// millisatoshis, never violates one.
//
// So the budget sets the rate. A payment with 10,000 millisatoshis left will
// pay 5,000 of them for one nat, which is a price a real route can exceed, and
// the search starts declining expensive reliability on its own rather than
// discovering the limit when the route is rejected. The rate is absolute, so it
// tightens in relative terms as the payment grows, which is the right
// direction: a fee budget quoted in parts per million bites hardest in absolute
// terms on the largest payments.
//
// NOTE: this must only be called for a payment that has a budget. It does not
// classify, and handing it the remainder of an absent budget would produce a
// perfectly plausible looking rate for a limit nobody set.
func intervalBudgetPrice(remaining lnwire.MilliSatoshi) float64 {
	price := float64(remaining) / intervalBudgetShare

	return math.Min(
		math.Max(price, intervalMinFeePrice), intervalMaxFeePrice,
	)
}

// intervalBudgeted reports whether a payment carries a fee budget that a route
// could exceed. Anything short of the sentinel is a real limit.
//
// NOTE: this belongs on the limit a payment was created with, and nowhere else.
// Applied to a remaining budget it answers a different question and gets it
// wrong, because a limit that has had fees subtracted from it no longer looks
// like the sentinel even when there was never a limit to begin with.
func intervalBudgeted(feeLimit lnwire.MilliSatoshi) bool {
	return feeLimit != lnwire.MaxMilliSatoshi
}

// intervalLabel is one Pareto-incomparable way of reaching the target from a
// node. The stock path finder keeps a single best distance per node, which is
// enough when the only thing being minimized is a scalar cost. Here it is not
// enough: because the search runs backwards and fees accrue along the way, a
// route that is cheaper but carries a larger amount is genuinely incomparable
// to one that is dearer but carries less, since the larger amount may be
// refused further upstream. A label carries all three of the quantities that
// decide that comparison.
type intervalLabel struct {
	// node is the node this label describes a route from.
	node route.Vertex

	// netAmountReceived is the amount this node needs to receive, with its
	// own inbound fee already subtracted.
	netAmountReceived lnwire.MilliSatoshi

	// outboundFee is the fee this node charges for the hop it forwards
	// over, which is needed to keep a negative inbound fee from taking a
	// node's total fee below zero.
	outboundFee lnwire.MilliSatoshi

	// incomingCltv is the expiry the incoming HTLC to this node carries.
	incomingCltv int32

	// routingInfoSize is the accumulated onion payload size of the route
	// from this node onwards.
	routingInfoSize uint64

	// score is the accumulated cost of the route from this node to the
	// target, and is what the search minimizes.
	score float64

	// risk is the accumulated negative log probability of the route, kept
	// apart from the score so that the caller can price a shard on
	// probability alone.
	risk float64

	// hops is the number of hops from this node to the target.
	hops uint16

	// edge is the hop out of this node, and child is the label it leads to.
	edge  *unifiedEdge
	child *intervalLabel

	// active is cleared when this label is dominated by another, at which
	// point any copy of it still sitting in the heap is skipped.
	active bool
}

// contains reports whether the given node already appears on the route this
// label describes, which is how the search avoids walking in circles.
func (l *intervalLabel) contains(node route.Vertex) bool {
	for current := l; current != nil; current = current.child {
		if current.node == node {
			return true
		}
	}

	return false
}

// rank scores a label for eviction. It folds the amount and the hop count into
// the score so that the label a node gives up is the one least likely to be
// part of a good route.
func (l *intervalLabel) rank(deliver lnwire.MilliSatoshi) float64 {
	amountRatio := math.Max(
		float64(l.netAmountReceived)/float64(deliver), 1,
	)

	return l.score + intervalLabelAmountWeight*math.Log(amountRatio) +
		intervalLabelHopWeight*float64(l.hops)
}

// intervalHeap is a min-heap of labels ordered by score.
type intervalHeap []*intervalLabel

func (h intervalHeap) Len() int { return len(h) }

func (h intervalHeap) Less(i, j int) bool { return h[i].score < h[j].score }

func (h intervalHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *intervalHeap) Push(value any) {
	*h = append(*h, value.(*intervalLabel))
}

func (h *intervalHeap) Pop() any {
	old := *h
	last := len(old) - 1
	item := old[last]
	old[last] = nil
	*h = old[:last]

	return item
}

// intervalFrontier holds the labels a node has kept, bounded in size.
type intervalFrontier struct {
	labels    map[route.Vertex][]*intervalLabel
	maxLabels int

	// keepCheapest protects the cheapest label a node holds from eviction.
	// It is set for a payment that carries a fee budget and cleared for one
	// that does not, which is a distinction the measurements insisted on.
	// See insert for what goes wrong when it is set unconditionally.
	keepCheapest bool
}

// insert files a label under its node, dropping it if an existing label already
// dominates it and dropping any existing labels it dominates in turn. It
// reports whether the label was kept.
func (f *intervalFrontier) insert(label *intervalLabel,
	deliver lnwire.MilliSatoshi) bool {

	existing := f.labels[label.node]

	// A label is dominated only when another label is no worse on all three
	// of score, amount and hop count. Anything less than that is a genuine
	// trade-off and both are kept.
	for _, old := range existing {
		if old.active &&
			old.score <= label.score+1e-12 &&
			old.netAmountReceived <= label.netAmountReceived &&
			old.hops <= label.hops {

			return false
		}
	}

	kept := make([]*intervalLabel, 0, len(existing)+1)
	for _, old := range existing {
		if !old.active {
			continue
		}

		if label.score <= old.score+1e-12 &&
			label.netAmountReceived <= old.netAmountReceived &&
			label.hops <= old.hops {

			old.active = false

			continue
		}

		kept = append(kept, old)
	}

	kept = append(kept, label)
	if len(kept) > f.maxLabels {
		// When the payment carries a fee budget, the cheapest label a
		// node holds is kept whatever its score, because it is the one
		// that survives if the budget binds. The amount a label needs to
		// receive is the fee it has accumulated plus the amount being
		// delivered, so the smallest of those is the cheapest route out
		// of this node. Without this the frontier fills with reliable
		// expensive labels and a payment that cannot afford them is left
		// with nothing to fall back to.
		//
		// When there is no budget the protection is dropped, because a
		// label kept for a budget that does not exist displaces a better
		// label for the payment actually being made. Measurement was
		// blunt about it: keeping the cheapest label unconditionally
		// cost 0.032 of objective on the out of distribution tier, all
		// of it in success rather than attempts, and the same shape
		// showed up on the unbudgeted economic control. The budgeted
		// tiers are where the keep earns its place, so that is where it
		// applies.
		protected := -1
		if f.keepCheapest {
			protected = 0
			for i := 1; i < len(kept); i++ {
				if kept[i].netAmountReceived <
					kept[protected].netAmountReceived {

					protected = i
				}
			}
		}

		worst := -1
		worstRank := 0.0
		for i := range kept {
			if i == protected {
				continue
			}

			if rank := kept[i].rank(deliver); worst < 0 ||
				rank > worstRank {

				worst = i
				worstRank = rank
			}
		}

		// If the label we were handed is the worst of the set, there is
		// no room for it.
		if worst < 0 || kept[worst] == label {
			return false
		}

		kept[worst].active = false
		kept = append(kept[:worst], kept[worst+1:]...)
	}

	label.active = true
	f.labels[label.node] = kept

	return true
}

// intervalEdgeProbability is the callback the search uses to price a hop. It is
// handed the directed channel the hop would use, the amount that would be sent
// over it, and the channel's capacity.
type intervalEdgeProbability func(key IntervalKey, amt,
	capacity lnwire.MilliSatoshi) float64

// intervalPathParams gathers everything the interval path finder needs for one
// search. It mirrors the arguments of the stock path finder, with the
// probability source replaced by one that reads the interval store rather than
// mission control.
type intervalPathParams struct {
	// graph carries the channel graph, the ephemeral edges and the
	// bandwidth hints for our own channels.
	graph *graphParams

	// restrictions are the constraints the route must respect, and are the
	// same ones the stock path finder is given.
	restrictions *RestrictParams

	// cfg holds the search bounds.
	cfg *IntervalConfig

	// probability prices a single hop.
	probability intervalEdgeProbability

	// self is our own node, source the node the route starts at and target
	// the node it ends at.
	self, source, target route.Vertex

	// amt is the amount to deliver to the target.
	amt lnwire.MilliSatoshi

	// finalHtlcExpiry is the absolute expiry height of the final hop.
	finalHtlcExpiry int32

	// cache holds the graph reads that do not depend on the amount, so that
	// every rung of a shard ladder pays for them once between them rather
	// than once each.
	cache *intervalGraphCache

	// feeRate says how this payment converts fees into the nats the score is
	// denominated in.
	feeRate intervalFeeRate
}

// intervalGraphCache holds what a search learns from the graph that does not
// change with the amount being routed. It is shared by every search of a single
// call to RequestRoute, which holds a graph session open across all of them, so
// the graph cannot move underneath it.
type intervalGraphCache struct {
	// unifiers holds the channels into a node, keyed by the node they come
	// from. Building these is the part of the search that touches the graph
	// database, so caching them is what makes pricing a whole shard ladder
	// affordable.
	unifiers map[route.Vertex]map[route.Vertex]*edgeUnifier

	// features holds the validated feature vector of a node, with a nil
	// entry meaning the node cannot be routed through.
	features map[route.Vertex]*lnwire.FeatureVector
}

// newIntervalGraphCache builds an empty cache.
func newIntervalGraphCache() *intervalGraphCache {
	return &intervalGraphCache{
		unifiers: make(
			map[route.Vertex]map[route.Vertex]*edgeUnifier,
		),
		features: make(map[route.Vertex]*lnwire.FeatureVector),
	}
}

// siblingCount returns how many channels connect the given directed pair, or
// zero when the search never looked at that pair. It is what decides whether an
// observation about a hop is allowed to name a channel.
func (c *intervalGraphCache) siblingCount(from, to route.Vertex) int {
	unifiers, ok := c.unifiers[to]
	if !ok {
		return 0
	}

	unifier, ok := unifiers[from]
	if !ok {
		return 0
	}

	return len(unifier.edges)
}

// findIntervalPath searches for a route from source to target able to deliver
// amt, scoring hops with the interval belief model. Like the stock path finder
// it searches backwards from the target so that fees and amounts accumulate in
// the direction they are actually paid, and it returns the path in forward
// order along with the negative log probability of the route.
//
// The search is label setting rather than shortest path: each node keeps a
// bounded set of Pareto-incomparable ways of reaching the target, so a route
// carrying a smaller amount at a higher cost survives alongside a cheaper one
// that carries more.
func findIntervalPath(ctx context.Context, p *intervalPathParams) (
	[]*unifiedEdge, float64, error) {

	features, err := intervalDestFeatures(ctx, p)
	if err != nil {
		return nil, 0, err
	}

	// Set up outgoing channel map for quicker access.
	var outgoingChanMap map[uint64]struct{}
	if len(p.restrictions.OutgoingChannelIDs) > 0 {
		outgoingChanMap = make(map[uint64]struct{})
		for _, outChan := range p.restrictions.OutgoingChannelIDs {
			outgoingChanMap[outChan] = struct{}{}
		}
	}

	// Build the reverse lookup of the ephemeral edges, since the search
	// runs from the target back towards us.
	additionalEdgesWithSrc := make(map[route.Vertex][]*edgePolicyWithSource)
	for vertex, edges := range p.graph.additionalEdges {
		if vertex == p.self {
			continue
		}

		for _, edge := range edges {
			policy := edge.EdgePolicy()
			toVertex := policy.ToNodePubKey()

			additionalEdgesWithSrc[toVertex] = append(
				additionalEdgesWithSrc[toVertex],
				&edgePolicyWithSource{
					sourceNode: vertex,
					edge:       edge,
				},
			)
		}
	}

	lastHopSize, err := lastHopPayloadSize(
		p.restrictions, p.finalHtlcExpiry, p.amt,
	)
	if err != nil {
		return nil, 0, err
	}

	// The search starts at the target, which needs to receive the full
	// amount and charges nothing.
	root := &intervalLabel{
		node:              p.target,
		netAmountReceived: p.amt,
		incomingCltv:      p.finalHtlcExpiry,
		routingInfoSize:   lastHopSize,
		active:            true,
	}

	queue := &intervalHeap{root}
	frontier := &intervalFrontier{
		labels:    map[route.Vertex][]*intervalLabel{},
		maxLabels: p.cfg.MaxLabels,
		// Protecting the cheapest label is worth a slot for a payment
		// whose budget could bind, and a cost for one with no budget to
		// bind, so the answer is whether it carries a limit at all.
		keepCheapest: p.feeRate.budgeted,
	}

	// Calculate the absolute cltv limit. Use uint64 to prevent an overflow
	// if the cltv limit is MaxUint32.
	absoluteCltvLimit := uint64(p.restrictions.CltvLimit) +
		uint64(p.finalHtlcExpiry)

	cache := p.cache
	if cache == nil {
		cache = newIntervalGraphCache()
	}

	search := &intervalSearch{
		params:            p,
		frontier:          frontier,
		queue:             queue,
		outgoingChanMap:   outgoingChanMap,
		additionalEdges:   additionalEdgesWithSrc,
		absoluteCltvLimit: absoluteCltvLimit,
		cache:             cache,
	}

	best, err := search.run(ctx)
	if err != nil {
		return nil, 0, err
	}
	if best == nil {
		return nil, 0, errNoPathFound
	}

	// Unravel the label chain into a forward ordered path.
	var pathEdges []*unifiedEdge
	for current := best; current != nil && current.edge != nil; {
		pathEdges = append(pathEdges, current.edge)
		current = current.child
	}

	// The final hop's features are the ones we validated above, which may
	// come from the invoice rather than from the graph.
	pathEdges[len(pathEdges)-1].policy.ToNodeFeatures = features

	return pathEdges, best.risk, nil
}

// intervalDestFeatures resolves and validates the feature vector of the payment
// destination, exactly as the stock path finder does.
func intervalDestFeatures(ctx context.Context, p *intervalPathParams) (
	*lnwire.FeatureVector, error) {

	features := p.restrictions.DestFeatures
	if features == nil {
		var err error
		features, err = p.graph.graph.FetchNodeFeatures(ctx, p.target)
		if err != nil {
			return nil, err
		}
	}

	if err := feature.ValidateRequired(features); err != nil {
		log.Warnf("Interval pathfinding destination features: %v", err)

		return nil, errUnknownRequiredFeature
	}

	if err := feature.ValidateDeps(features); err != nil {
		log.Warnf("Interval pathfinding destination features: %v", err)

		return nil, errMissingDependentFeature
	}

	if p.restrictions.PaymentAddr.IsSome() &&
		!features.HasFeature(lnwire.PaymentAddrOptional) {

		return nil, errNoPaymentAddr
	}

	return features, nil
}

// intervalSearch holds the mutable state of one path finding run.
type intervalSearch struct {
	params   *intervalPathParams
	frontier *intervalFrontier
	queue    *intervalHeap

	outgoingChanMap   map[uint64]struct{}
	additionalEdges   map[route.Vertex][]*edgePolicyWithSource
	absoluteCltvLimit uint64

	// cache holds the graph reads shared with every other search of the
	// same route request.
	cache *intervalGraphCache

	expansions int
}

// run walks the graph until it pops a label at the source node, exhausts the
// queue, or runs out of its expansion budget.
func (s *intervalSearch) run(ctx context.Context) (*intervalLabel, error) {
	p := s.params

	for s.queue.Len() != 0 {
		label := heap.Pop(s.queue).(*intervalLabel)
		if !label.active {
			continue
		}

		// Reaching the source means we have a complete route. Because
		// the queue is ordered by score, the first one we pop is the
		// cheapest. The hop count guard is what keeps a payment to
		// ourselves from terminating on its own starting label.
		if label.node == p.source && label.hops > 0 {
			return label, nil
		}

		if label.hops >= p.cfg.MaxRouteHops {
			continue
		}

		s.expansions++
		if s.expansions > p.cfg.SearchLimit {
			log.Debugf("Interval pathfinding hit its expansion "+
				"budget of %v", p.cfg.SearchLimit)

			break
		}

		if err := s.expand(ctx, label); err != nil {
			return nil, err
		}
	}

	return nil, nil
}

// expand walks every channel into the label's node and files the labels they
// produce.
func (s *intervalSearch) expand(ctx context.Context,
	label *intervalLabel) error {

	p := s.params

	unifiers, err := s.incomingEdges(label.node)
	if err != nil {
		return err
	}

	routeToSelf := p.source == p.target

	for fromNode, unifier := range unifiers {
		// The target is where the search started, so walking back into
		// it would close a loop. The one exception is a payment to
		// ourselves, which is a loop by construction.
		if !routeToSelf && fromNode == p.target {
			continue
		}

		// Apply the last hop restriction if one is set.
		if p.restrictions.LastHop != nil && label.node == p.target &&
			fromNode != *p.restrictions.LastHop {

			continue
		}

		// The source node is always allowed, since the search stops
		// there; anything else already on the route would be a cycle.
		if fromNode != p.source && label.contains(fromNode) {
			continue
		}

		edge := unifier.getEdge(
			label.netAmountReceived, p.graph.bandwidthHints,
			label.outboundFee,
		)
		if edge == nil {
			continue
		}

		features, err := s.nodeFeatures(ctx, fromNode)
		if err != nil {
			return err
		}
		if features == nil {
			continue
		}

		// How many channels this pair has decides whether an
		// observation about the hop can name one of them.
		s.processEdge(fromNode, edge, label, len(unifier.edges))
	}

	return nil
}

// processEdge prices one candidate hop and files the label it produces if the
// hop respects every restriction and is not dominated by a label the node
// already holds.
func (s *intervalSearch) processEdge(fromNode route.Vertex, edge *unifiedEdge,
	label *intervalLabel, siblings int) {

	p := s.params

	// Calculate the inbound fee charged by the node we are walking back
	// from, keeping its total fee from going negative.
	inboundFee := edge.inboundFees.CalcFee(label.netAmountReceived)
	if minInboundFee := -int64(label.outboundFee); inboundFee <
		minInboundFee {

		inboundFee = minInboundFee
	}

	// This is the amount the candidate node would have to send onwards.
	amountToSend := label.netAmountReceived +
		lnwire.MilliSatoshi(inboundFee)

	// Refuse to build a route whose accumulated fee runs past the budget.
	totalFee := int64(amountToSend) - int64(p.amt)
	if totalFee > 0 &&
		lnwire.MilliSatoshi(totalFee) > p.restrictions.FeeLimit {

		return
	}

	probability := p.probability(
		intervalScopeKey(IntervalKey{
			ChanID: edge.policy.ChannelID,
			From:   fromNode,
			To:     label.node,
		}, siblings),
		amountToSend, lnwire.NewMSatFromSatoshis(edge.capacity),
	)
	if probability <= 0 {
		return
	}

	// The source node has no predecessor to charge a fee or a time lock.
	var (
		timeLockDelta uint16
		outboundFee   int64
	)
	if fromNode != p.source {
		outboundFee = int64(edge.policy.ComputeFee(amountToSend))
		timeLockDelta = edge.policy.TimeLockDelta
	}

	incomingCltv := label.incomingCltv + int32(timeLockDelta)
	if uint64(incomingCltv) > s.absoluteCltvLimit {
		return
	}

	// Refuse to build an onion the network will not carry.
	routingInfoSize := label.routingInfoSize
	if fromNode != p.source {
		if edge.hopPayloadSizeFn == nil {
			log.Criticalf("No payload size function available for "+
				"edge=%v: %v", edge, ErrNoPayLoadSizeFunc)

			return
		}

		routingInfoSize += edge.hopPayloadSizeFn(
			amountToSend, uint32(label.incomingCltv),
			edge.policy.ChannelID,
		)
	}
	if routingInfoSize > sphinx.MaxRoutingPayloadSize {
		return
	}

	// With the hop accepted, price it. The dominant term is the negative
	// log of the probability, which is what makes the cost additive over
	// hops in the first place.
	signedFee := inboundFee + outboundFee
	fee := float64(0)
	if signedFee > 0 {
		fee = float64(signedFee)
	}

	edgeRisk := -math.Log(probability)
	feePenalty := p.feeRate.penalty(fee, p.amt, intervalFeeWeight)
	hopPenalty := intervalHopBase +
		intervalHopGrowth*float64(label.hops)

	capacityPenalty := float64(0)
	capacity := lnwire.NewMSatFromSatoshis(edge.capacity)
	if capacity > 0 {
		ratio := float64(amountToSend) / float64(capacity)
		if ratio > intervalCapacityKnee {
			over := (ratio - intervalCapacityKnee) /
				(1 - intervalCapacityKnee)
			capacityPenalty = intervalCapacityWeight * over * over
		}
	}

	candidate := &intervalLabel{
		node:              fromNode,
		netAmountReceived: amountToSend + lnwire.MilliSatoshi(outboundFee),
		outboundFee:       lnwire.MilliSatoshi(outboundFee),
		incomingCltv:      incomingCltv,
		routingInfoSize:   routingInfoSize,
		score: label.score + edgeRisk + feePenalty + hopPenalty +
			capacityPenalty,
		risk:  label.risk + edgeRisk,
		hops:  label.hops + 1,
		edge:  edge,
		child: label,
	}

	// A label at the source is a finished route, so it goes straight onto
	// the queue rather than into a frontier it would never be expanded
	// from.
	if fromNode == p.source {
		candidate.active = true
		heap.Push(s.queue, candidate)

		return
	}

	if !s.frontier.insert(candidate, p.amt) {
		return
	}

	heap.Push(s.queue, candidate)
}

// incomingEdges returns the channels into the given node, keyed by the node
// they come from, building and caching them on first use.
func (s *intervalSearch) incomingEdges(node route.Vertex) (
	map[route.Vertex]*edgeUnifier, error) {

	if cached, ok := s.cache.unifiers[node]; ok {
		return cached, nil
	}

	p := s.params

	// The exit hop does not charge an inbound fee.
	isExitHop := node == p.target

	u := newNodeEdgeUnifier(p.self, node, !isExitHop, s.outgoingChanMap)
	if err := u.addGraphPolicies(p.graph.graph); err != nil {
		return nil, err
	}

	// Fold in any ephemeral edges that lead to this node. Hop hints carry
	// no capacity, so we assume a large one, the same way the stock path
	// finder does.
	for _, reverseEdge := range s.additionalEdges[node] {
		u.addPolicy(
			reverseEdge.sourceNode, reverseEdge.edge.EdgePolicy(),
			models.InboundFee{}, fakeHopHintCapacity,
			reverseEdge.edge.IntermediatePayloadSize,
			reverseEdge.edge.BlindedPayment(),
		)
	}

	s.cache.unifiers[node] = u.edgeUnifiers

	return u.edgeUnifiers, nil
}

// nodeFeatures returns the validated feature vector of a node, or nil if the
// node cannot be routed through.
func (s *intervalSearch) nodeFeatures(ctx context.Context,
	node route.Vertex) (*lnwire.FeatureVector, error) {

	if cached, ok := s.cache.features[node]; ok {
		return cached, nil
	}

	features, err := s.params.graph.graph.FetchNodeFeatures(ctx, node)
	if err != nil {
		return nil, err
	}

	// Do not route through nodes that require features we do not know, or
	// that fail to set their transitive dependencies.
	if err := feature.ValidateRequired(features); err != nil {
		s.cache.features[node] = nil

		return nil, nil
	}
	if err := feature.ValidateDeps(features); err != nil {
		s.cache.features[node] = nil

		return nil, nil
	}

	s.cache.features[node] = features

	return features, nil
}
