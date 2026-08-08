package routing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/btcsuite/btclog/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// The weights below price a candidate shard once a route has been found for it.
// The stock session never makes this comparison, because it does not choose a
// shard size: it asks for the whole remaining amount and halves it whenever no
// route comes back. This session prices every rung of a ladder and takes the
// best, which is what lets it split before it has failed at all.
const (
	// intervalShardFeeWeight sets the fee sensitivity of the shard score
	// when the payment carries no fee budget, in the same units and for the
	// same reasons as intervalFeeWeight. See intervalFeePenalty.
	intervalShardFeeWeight = 4.0

	// intervalShardHopWeight prices each hop of the shard's route, which
	// breaks ties towards the shorter of two otherwise equal routes.
	intervalShardHopWeight = 0.006

	// intervalCompletionBonus is added to a shard that covers the whole
	// remaining amount. Finishing a payment in one HTLC is worth a little
	// more than the sum of its parts suggests, because every extra shard is
	// another chance to fail and another HTLC to hold open.
	intervalCompletionBonus = 0.08

	// The split appetite weights below decide how much a larger shard is
	// worth relative to its risk, and they respond to how the payment is
	// going. Once a part has settled we are committed and want to finish;
	// after several failures we would rather cut smaller and make progress.
	intervalAppetiteDefault    = 0.72
	intervalAppetiteCommitted  = 0.94
	intervalAppetiteStruggling = 0.50
	intervalAppetiteCautious   = 0.60

	// intervalStrugglingAfter is the number of failed attempts after which
	// the session turns cautious about shard size.
	intervalStrugglingAfter = 3
)

// Session penalties. These are per payment and die with it, unlike the interval
// store, which outlives every payment. A penalty here says "this payment has a
// reason to avoid this channel", not "this channel is bad".
const (
	// intervalFailurePenalty is applied to a channel this payment watched
	// fail.
	intervalFailurePenalty = 1.35

	// intervalPolicyPenalty is applied to a channel that rejected the HTLC
	// on a policy grounds, such as an insufficient fee or a bad expiry.
	// Such a channel is also blocked for the rest of the payment, since the
	// policy we hold for it is stale and a retry would be built from the
	// same stale policy.
	intervalPolicyPenalty = 6

	// intervalUnknownPenalty is applied to a channel that failed for a
	// reason we have no model for.
	intervalUnknownPenalty = 3

	// intervalContradictionPenalty is applied across a whole route when an
	// unattributable failure names no suspect at all, which means something
	// we believe is wrong but we cannot tell what.
	intervalContradictionPenalty = 0.35

	// intervalRoutePenalty is applied across a whole route on a failure
	// that carries no liquidity information.
	intervalRoutePenalty = 1.0

	// intervalSuspicionMass is spread across the suspects of an
	// unattributable failure, divided by the square root of how many there
	// are, so that a failure with one plausible cause speaks far louder
	// than one with ten.
	intervalSuspicionMass = 2.2

	// intervalSuspicionEscalation and intervalSuspicionCertainty are the
	// counts at which a channel that keeps turning up in unexplained
	// failures earns an extra penalty and then a hard bound. This is a
	// Bayesian argument in the shape of a counter.
	intervalSuspicionEscalation = 4
	intervalSuspicionCertainty  = 8

	// intervalExtraSuspicionPenalty is the extra penalty applied at the
	// escalation threshold.
	intervalExtraSuspicionPenalty = 0.30

	// intervalPenaltyWeight converts an accumulated penalty into a
	// probability multiplier, and intervalPenaltyCap bounds how much
	// penalty can pile onto one channel.
	intervalPenaltyWeight = 0.70
	intervalPenaltyCap    = 8

	// intervalSettleDecay and intervalProbeDecay are the factors a penalty
	// is multiplied by when the channel proves itself, by settling a part
	// or by forwarding one. A channel that works is forgiven quickly, but
	// not instantly.
	intervalSettleDecay = 0.12
	intervalProbeDecay  = 0.25

	// intervalPenaltyFloor is the level below which a decayed penalty is
	// dropped rather than kept around at a value that no longer matters.
	intervalPenaltyFloor = 0.25
)

// intervalShardDivisors are the divisors applied to an amount this payment has
// already proven does not fit, to produce shard sizes that sit just under it.
// This is what makes the ladder a function of the beliefs and not only of the
// amount.
var intervalShardDivisors = []lnwire.MilliSatoshi{2, 4, 8, 16, 32}

// intervalShardMultiples are small multiples of the smallest shard that would
// still let the payment finish within its part budget.
var intervalShardMultiples = []lnwire.MilliSatoshi{2, 3, 4, 6, 8}

// intervalPaymentSession is a PaymentSession that owns route selection and MPP
// splitting together. Where the stock session asks path finding for the whole
// remaining amount and halves it when nothing comes back, this one enumerates a
// ladder of candidate shard sizes, finds a route for each, and picks the best
// pairing of the two. Both halves of that decision read the same liquidity
// intervals, so a failure at one amount reshapes not just which route is tried
// next but which amounts are considered at all.
type intervalPaymentSession struct {
	additionalEdges

	selfNode route.Vertex

	payment *LightningPayment

	getBandwidthHints func(Graph) (bandwidthHints, error)

	graphSessFactory GraphSessionFactory

	// store is the node wide belief about channel liquidity, shared with
	// every other payment.
	store *IntervalStore

	cfg IntervalConfig

	log btclog.Logger

	// mu guards everything below it. RequestRoute and the result reporting
	// methods are all driven from the payment lifecycle's own goroutine,
	// but the session's contract says it is safe for concurrent access, so
	// we make it so.
	mu sync.Mutex

	// penalties, blocked, failedAt and suspects are this payment's own view
	// of the channels it has touched, and they die with the payment.
	penalties map[IntervalKey]float64
	blocked   map[IntervalKey]struct{}
	failedAt  map[IntervalKey]lnwire.MilliSatoshi
	suspects  map[IntervalKey]uint32

	// routeFailedAt remembers the smallest amount at which a given route
	// has failed, so that the same route is not handed out again at an
	// amount we already know it cannot carry.
	routeFailedAt map[string]lnwire.MilliSatoshi

	// capacities remembers the capacity we path found each directed channel
	// against, so that an outcome reported later can be recorded against
	// the same scale it was priced with.
	capacities map[IntervalKey]lnwire.MilliSatoshi

	// scopes remembers, for each hop of a route we dispatched, the key the
	// hop was priced under. A hop across a pair with several channels is
	// priced and recorded at pair scope, because nothing in an onion
	// failure says which of the channels carried the payment.
	scopes map[IntervalKey]IntervalKey

	// outstanding holds the shards this session has handed to the payment
	// lifecycle and not yet seen resolved, oldest first. Every entry is
	// mirrored into the node wide overlay, so this slice is also the record
	// of what this session owes back to it.
	outstanding []*heldShard

	// budgeted is latched at construction from the fee limit this payment
	// was created with. It decides which way fees are priced and whether the
	// search protects the cheapest label, and it must never be re-derived
	// from the remaining budget the lifecycle hands RequestRoute.
	budgeted bool

	attempts       uint32
	failedAttempts uint32
	settledParts   uint32
}

// heldShard is one route this session returned, and the amounts it committed
// on each directed channel of that route.
type heldShard struct {
	// routeKey identifies the route, so that an outcome reported later can
	// be matched back to the shard that produced it.
	routeKey string

	// amounts is what the shard committed, keyed at the same scope the
	// route was priced under.
	amounts map[IntervalKey]lnwire.MilliSatoshi
}

// A compile time assertion to ensure the interval session satisfies both the
// session contract and the optional result reporting one.
var _ PaymentSession = (*intervalPaymentSession)(nil)
var _ PaymentResultReporter = (*intervalPaymentSession)(nil)

// newIntervalPaymentSession builds a session for one payment.
func newIntervalPaymentSession(p *LightningPayment, selfNode route.Vertex,
	getBandwidthHints func(Graph) (bandwidthHints, error),
	graphSessFactory GraphSessionFactory, store *IntervalStore,
	cfg IntervalConfig) (*intervalPaymentSession, error) {

	edges, err := RouteHintsToEdges(p.RouteHints, p.Target)
	if err != nil {
		return nil, err
	}

	cfg.fillDefaults()

	logPrefix := fmt.Sprintf("IntervalSession(%x):", p.Identifier())

	return &intervalPaymentSession{
		// Whether this payment has a budget is settled here and never
		// asked again. RequestRoute is handed the budget remaining
		// rather than the budget, and a limit with fees subtracted from
		// it stops looking like the no-limit sentinel after the first
		// shard pays anything, so a payment with no budget that splits
		// would otherwise reclassify itself as having one.
		budgeted:          intervalBudgeted(p.FeeLimit),
		additionalEdges:   edges,
		selfNode:          selfNode,
		payment:           p,
		getBandwidthHints: getBandwidthHints,
		graphSessFactory:  graphSessFactory,
		store:             store,
		cfg:               cfg,
		log:               log.WithPrefix(logPrefix),
		penalties:         make(map[IntervalKey]float64),
		blocked:           make(map[IntervalKey]struct{}),
		failedAt:          make(map[IntervalKey]lnwire.MilliSatoshi),
		suspects:          make(map[IntervalKey]uint32),
		routeFailedAt:     make(map[string]lnwire.MilliSatoshi),
		capacities:        make(map[IntervalKey]lnwire.MilliSatoshi),
		scopes:            make(map[IntervalKey]IntervalKey),
	}, nil
}

// intervalChoice is one candidate pairing of a shard size with the route found
// for it.
type intervalChoice struct {
	route   *route.Route
	edges   []*unifiedEdge
	cache   *intervalGraphCache
	shard   lnwire.MilliSatoshi
	utility float64
}

// RequestRoute returns the next route to attempt, which may carry the whole
// remaining amount or only a shard of it.
//
// NOTE: This function is safe for concurrent access.
// NOTE: Part of the PaymentSession interface.
func (p *intervalPaymentSession) RequestRoute(maxAmt,
	feeLimit lnwire.MilliSatoshi, activeShards, height uint32,
	firstHopCustomRecords lnwire.CustomRecords) (*route.Route, error) {

	p.mu.Lock()
	defer p.mu.Unlock()

	if maxAmt == 0 {
		return nil, errNoPathFound
	}

	// The lifecycle reads the number of HTLCs in flight from the payments
	// database, so it is the one count we can trust. Reconcile our own
	// record of what we are holding against it before pricing anything.
	p.reconcileHolds(activeShards)

	// A session that believes it can always find one more route would
	// otherwise spin until the payment times out.
	if p.attempts >= p.cfg.AttemptLimit {
		p.log.Debugf("Giving up after %v attempts", p.attempts)

		return nil, errNoPathFound
	}

	// Respect the client side maximum shard size if one is set.
	if p.payment.MaxShardAmt != nil && maxAmt > *p.payment.MaxShardAmt {
		p.log.Debugf("Clamping payment attempt from %v to %v due to "+
			"max shard size of %v", maxAmt, *p.payment.MaxShardAmt,
			*p.payment.MaxShardAmt)

		maxAmt = *p.payment.MaxShardAmt
	}

	// Add BlockPadding to the finalCltvDelta so that the receiving node
	// does not reject the HTLC if some blocks are mined while it's in
	// flight.
	finalCltvDelta := p.payment.FinalCLTVDelta + BlockPadding

	// The final delta is subtracted before path finding, because the
	// optimal path does not depend on it.
	restrictions := &RestrictParams{
		FeeLimit:              feeLimit,
		OutgoingChannelIDs:    p.payment.OutgoingChannelIDs,
		LastHop:               p.payment.LastHop,
		CltvLimit:             p.payment.CltvLimit - uint32(finalCltvDelta),
		DestCustomRecords:     p.payment.DestCustomRecords,
		DestFeatures:          p.payment.DestFeatures,
		PaymentAddr:           p.payment.PaymentAddr,
		Amp:                   p.payment.amp,
		Metadata:              p.payment.Metadata,
		FirstHopCustomRecords: firstHopCustomRecords,
	}

	finalHtlcExpiry := int32(height) + int32(finalCltvDelta)

	partsLeft := p.partsLeft(activeShards)
	if partsLeft == 0 {
		p.log.Debugf("Not requesting a route, the part limit of %v "+
			"has been reached", p.payment.MaxParts)

		return nil, errNoPathFound
	}

	// The smallest shard that would still let the remaining amount be
	// delivered within the parts we have left.
	minimum := intervalCeilDiv(maxAmt, partsLeft)
	shards := p.shardAmounts(maxAmt, minimum, partsLeft)

	request := &intervalShardRequest{
		restrictions:    restrictions,
		shards:          shards,
		maxAmt:          maxAmt,
		minimum:         minimum,
		finalCltvDelta:  finalCltvDelta,
		finalHtlcExpiry: finalHtlcExpiry,
		height:          height,
	}

	var (
		best    *intervalChoice
		pathErr noRouteError
		found   bool
		ctx     = context.TODO()
	)

	findBest := func(graph graphdb.NodeTraverser) error {
		var err error
		best, pathErr, found, err = p.chooseShard(ctx, graph, request)

		return err
	}

	err := p.graphSessFactory.GraphSession(ctx, findBest, func() {
		best, found = nil, false
	})
	if err != nil {
		return nil, err
	}

	if best == nil {
		// Report the most informative error the ladder produced, so
		// that the payment is failed for the right reason.
		if found {
			return nil, pathErr
		}

		return nil, errNoPathFound
	}

	p.attempts++
	p.recordCapacities(best.edges, best.cache)
	p.holdRoute(best.route)

	p.log.Debugf("Attempting shard of %v out of %v remaining over %v hops",
		best.shard, maxAmt, len(best.route.Hops))

	return best.route, nil
}

// intervalShardRequest gathers what one call to RequestRoute needs to price
// every rung of its shard ladder.
type intervalShardRequest struct {
	// restrictions are the constraints every route must respect.
	restrictions *RestrictParams

	// shards is the ladder of candidate shard sizes.
	shards []lnwire.MilliSatoshi

	// maxAmt is the whole remaining amount of the payment, and minimum the
	// smallest shard that could still deliver it within the parts left.
	maxAmt, minimum lnwire.MilliSatoshi

	// finalCltvDelta is the expiry delta of the final hop, including the
	// block padding.
	finalCltvDelta uint16

	// finalHtlcExpiry is the absolute expiry height of the final hop, and
	// height the current block height.
	finalHtlcExpiry int32
	height          uint32
}

// chooseShard prices every rung of the shard ladder and returns the best
// pairing of shard and route. The second and third return values carry the most
// informative non-critical error the ladder produced, for the case where no
// rung produced a route at all.
func (p *intervalPaymentSession) chooseShard(ctx context.Context,
	graph graphdb.NodeTraverser, req *intervalShardRequest) (
	*intervalChoice, noRouteError, bool, error) {

	bandwidthHints, err := p.getBandwidthHints(graph)
	if err != nil {
		return nil, 0, false, err
	}

	// If our own channels cannot cover the remaining amount in total, no
	// arrangement of shards can complete the payment, and sending the parts
	// we can afford would only leave them held at the receiver.
	_, total, err := getOutgoingBalance(
		p.selfNode, p.outgoingChanMap(), bandwidthHints, graph,
	)
	if err != nil {
		return nil, 0, false, err
	}
	if total < req.maxAmt {
		p.log.Debugf("Local balance of %v cannot cover %v", total,
			req.maxAmt)

		return nil, errInsufficientBalance, true, nil
	}

	params := &intervalPathParams{
		graph: &graphParams{
			graph:           graph,
			additionalEdges: p.additionalEdges,
			bandwidthHints:  bandwidthHints,
		},
		restrictions:    req.restrictions,
		cfg:             &p.cfg,
		probability:     p.edgeProbability,
		self:            p.selfNode,
		source:          p.selfNode,
		target:          p.payment.Target,
		finalHtlcExpiry: req.finalHtlcExpiry,

		// Every rung of the ladder walks the same graph, and the graph
		// session held open around this loop means it cannot change
		// underneath us, so the reads that do not depend on the amount
		// are paid for once between all of them.
		cache: newIntervalGraphCache(),
	}

	// The appetite for a large shard depends on how the payment is going.
	appetite := intervalAppetiteDefault
	switch {
	case p.settledParts > 0:
		appetite = intervalAppetiteCommitted

	case p.failedAttempts >= intervalStrugglingAfter:
		appetite = intervalAppetiteStruggling

	case p.failedAttempts > 0:
		appetite = intervalAppetiteCautious
	}

	var (
		best    *intervalChoice
		pathErr noRouteError
		found   bool
	)

	// The budget belongs to the payment rather than to any one shard, so
	// every rung is priced against the same rate.
	params.feeRate = p.feeRate(req.restrictions.FeeLimit)

	for _, shard := range req.shards {
		params.amt = shard

		pathEdges, risk, err := findIntervalPath(ctx, params)
		if err != nil {
			var routeErr noRouteError
			if !errors.As(err, &routeErr) {
				return nil, 0, false, err
			}

			// An error about the destination applies to every rung
			// of the ladder, so there is no point walking the rest
			// of it.
			if routeErr != errNoPathFound {
				return nil, routeErr, true, nil
			}

			pathErr, found = routeErr, true

			continue
		}

		rt, err := newRoute(
			p.selfNode, pathEdges, req.height, finalHopParams{
				amt:         shard,
				totalAmt:    p.payment.Amount,
				cltvDelta:   req.finalCltvDelta,
				records:     p.payment.DestCustomRecords,
				paymentAddr: p.payment.PaymentAddr,
				metadata:    p.payment.Metadata,
			}, p.payment.BlindedPathSet,
		)
		if err != nil {
			return nil, 0, false, err
		}

		// Skip a route this payment has already watched fail at this
		// amount or a smaller one.
		if p.routeRejected(rt, shard) {
			continue
		}

		// Never hand out a route the payment cannot afford. The search
		// prunes on the same limit while it walks, so reaching this is a
		// sign that the route built from the path costs more than the
		// path did, and the safe answer is to drop the rung rather than
		// to spend an HTLC finding out.
		if fee := rt.TotalAmount - shard; fee > req.restrictions.FeeLimit {
			p.log.Debugf("Discarding a %v shard whose fee of %v "+
				"exceeds the remaining budget of %v", shard,
				fee, req.restrictions.FeeLimit)

			continue
		}

		choice := &intervalChoice{
			route: rt,
			edges: pathEdges,
			cache: params.cache,
			shard: shard,
			utility: intervalUtility(
				rt, shard, req.maxAmt, req.minimum,
				params.feeRate, risk, appetite,
			),
		}

		// Ties break towards the larger shard, which keeps the router
		// from cutting a payment finer than it has a reason to.
		switch {
		case best == nil:
			best = choice

		case choice.utility > best.utility+1e-12:
			best = choice

		case math.Abs(choice.utility-best.utility) <= 1e-12 &&
			choice.shard > best.shard:

			best = choice
		}
	}

	return best, pathErr, found, nil
}

// intervalUtility prices a shard and the route found for it. The dominant term
// is the risk of the route, traded against how much of the remaining amount the
// shard would carry.
func intervalUtility(rt *route.Route, shard, maxAmt,
	minimum lnwire.MilliSatoshi, feeRate intervalFeeRate,
	risk, appetite float64) float64 {

	progress := math.Log(math.Max(
		float64(shard)/math.Max(float64(minimum), 1), 1,
	))

	fee := rt.TotalAmount - shard
	feePenalty := feeRate.penalty(
		float64(fee), shard, intervalShardFeeWeight,
	)
	hopPenalty := intervalShardHopWeight * float64(len(rt.Hops))

	completionBonus := float64(0)
	if shard == maxAmt {
		completionBonus = intervalCompletionBonus
	}

	return -risk + appetite*progress + completionBonus - feePenalty -
		hopPenalty
}

// partsLeft returns how many more HTLCs this payment is allowed to have in
// flight, given how many it has now.
func (p *intervalPaymentSession) partsLeft(activeShards uint32) uint32 {
	maxParts := p.payment.MaxParts
	if maxParts == 0 {
		maxParts = 1
	}

	// Splitting also needs the receiver to be able to reassemble the parts.
	if !p.canSplit() {
		maxParts = 1
	}

	if activeShards >= maxParts {
		return 0
	}

	return maxParts - activeShards
}

// canSplit reports whether this payment may be cut into more than one HTLC. The
// conditions are the ones the stock session applies before it halves an amount:
// the receiver needs to be told what the parts add up to, which needs either a
// payment address or a blinded path, and it needs to understand MPP or AMP.
func (p *intervalPaymentSession) canSplit() bool {
	if p.payment.PaymentAddr.IsNone() && p.payment.BlindedPathSet == nil {
		return false
	}

	features := p.payment.DestFeatures
	if features == nil {
		return false
	}

	return features.HasFeature(lnwire.MPPOptional) ||
		features.HasFeature(lnwire.AMPOptional)
}

// outgoingChanMap returns the first hop restriction as a set, or nil when the
// payment does not restrict its first hop.
func (p *intervalPaymentSession) outgoingChanMap() map[uint64]struct{} {
	if len(p.payment.OutgoingChannelIDs) == 0 {
		return nil
	}

	chans := make(map[uint64]struct{}, len(p.payment.OutgoingChannelIDs))
	for _, chanID := range p.payment.OutgoingChannelIDs {
		chans[chanID] = struct{}{}
	}

	return chans
}

// intervalCeilDiv divides an amount by a divisor, rounding up, so that the
// resulting shards always add up to at least the amount.
func intervalCeilDiv(amt lnwire.MilliSatoshi,
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

// shardAmounts enumerates the shard sizes worth pricing for the given remaining
// amount. Four sources feed it: the amounts this payment has already proven do
// not fit, divided down until they do; the even division of the amount into a
// number of parts; the halving chain the stock session would walk one step at a
// time; and small multiples of the smallest usable shard.
//
// The first source is what makes the ladder a function of the beliefs rather
// than of the amount alone: a failure at some amount immediately puts shard
// sizes that sit just under it into play. It is enumerated first because every
// rung costs a full search, so when the ladder is cut short these are the rungs
// worth keeping.
func (p *intervalPaymentSession) shardAmounts(amt,
	minimum lnwire.MilliSatoshi, partsLeft uint32) []lnwire.MilliSatoshi {

	if partsLeft <= 1 {
		return []lnwire.MilliSatoshi{amt}
	}

	limit := partsLeft
	if limit > p.cfg.MaxShards {
		limit = p.cfg.MaxShards
	}

	var (
		seen    = make(map[lnwire.MilliSatoshi]struct{})
		amounts = make([]lnwire.MilliSatoshi, 0, p.cfg.MaxLadderRungs)
	)

	add := func(shard lnwire.MilliSatoshi) {
		if len(amounts) >= p.cfg.MaxLadderRungs {
			return
		}

		if shard == 0 || shard > amt || shard < minimum {
			return
		}

		// The whole remaining amount is always worth trying, but
		// anything smaller has to clear the minimum shard size, since
		// cutting below it produces HTLCs too small to be worth the
		// round trip.
		if shard != amt && shard < p.cfg.MinShardAmt {
			return
		}

		if _, ok := seen[shard]; ok {
			return
		}

		seen[shard] = struct{}{}
		amounts = append(amounts, shard)
	}

	add(amt)
	add(minimum)

	for _, failedAt := range p.failedAt {
		if failedAt <= 1 {
			continue
		}

		for _, divisor := range intervalShardDivisors {
			add((failedAt - 1) / divisor)
		}
	}

	for parts := uint32(2); parts <= limit; parts++ {
		add(intervalCeilDiv(amt, parts))
	}

	for shard := amt / 2; shard >= minimum && shard > 0; shard /= 2 {
		add(shard)

		if shard == minimum {
			break
		}
	}

	if minimum < amt {
		for _, multiple := range intervalShardMultiples {
			add(minimum * multiple)
		}
	}

	return amounts
}

// edgeProbability prices a single hop for the path finder. It layers this
// payment's own experience over the node wide belief: a channel this payment
// has watched fail is discounted by how much smaller the retry is, and one it
// has a reason to distrust is discounted by the penalty it has accumulated.
func (p *intervalPaymentSession) edgeProbability(key IntervalKey,
	amt, capacity lnwire.MilliSatoshi) float64 {

	if _, blocked := p.blocked[key]; blocked {
		return 0
	}

	var probability float64

	// Our own in-flight HTLCs have already committed part of what this edge
	// had when we last looked at it, so a new shard of amt needs the edge
	// to have held amt on top of what we are holding. Asking the model
	// about the sum is the whole of the adjustment: it needs no new term,
	// because every bound and every branch already answers the question
	// "was there this much here".
	//
	// The first hop is the exception. The switch nets our in-flight HTLCs
	// out of the bandwidth it reports for our own links, so the pathfinder
	// has already been told, and adding the hold here would charge the same
	// liquidity twice.
	effective := amt
	if key.From != p.selfNode {
		effective += p.store.Held(key)
	}

	failedAt := p.failedAt[key]
	retryFactor := intervalRetryFactor(effective, failedAt)
	if retryFactor == 0 {
		return 0
	}

	if key.From == p.selfNode {
		// We know our own balances exactly, and the bandwidth hints
		// have already refused any channel that cannot carry the
		// amount. The small haircut below certainty is what makes the
		// search prefer a shorter route without a separate term for it.
		probability = intervalLocalProbability
	} else {
		interval := p.store.Get(key, capacity)
		probability = interval.Probability(effective, capacity)

		// A retry below an amount we have proven passes is not a retry
		// at all, so the ladder does not apply to it.
		if interval.LowerOK >= effective {
			failedAt = 0
		}
	}

	if probability == 0 {
		return 0
	}

	if failedAt != 0 {
		probability *= retryFactor
	}

	if penalty := p.penalties[key]; penalty > 0 {
		probability *= math.Exp(
			-intervalPenaltyWeight *
				math.Min(penalty, intervalPenaltyCap),
		)
	}

	return math.Min(
		math.Max(probability, intervalMinProbability),
		intervalMaxProbability,
	)
}

// ReportAttemptSuccess folds a settled shard into both the session's own state
// and the node wide belief. A settlement is the only observation that moves
// liquidity rather than merely bounding it.
//
// NOTE: Part of the PaymentResultReporter interface.
func (p *intervalPaymentSession) ReportAttemptSuccess(_ uint64,
	rt *route.Route) {

	if rt == nil || len(rt.Hops) == 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// The HTLC has resolved, so whatever it was holding is no longer held.
	// The settlement recorded below moves the interval itself, which is how
	// the liquidity this shard actually spent leaves our picture of the
	// channel for good.
	p.releaseRoute(rt)

	p.settledParts++

	for i, key := range p.routeKeys(rt) {
		amt := intervalHopAmount(rt, i)

		if key.From != p.selfNode {
			p.store.RecordSettlement(key, amt, p.capacities[key])
		}

		// The session's own bounds describe the same channel, so they
		// move with it.
		p.shiftSessionLiquidity(key, amt)

		// A channel that just carried a part has earned back most of
		// the suspicion this payment placed on it.
		p.decayPenalty(key, intervalSettleDecay)
		if p.suspects[key] > 1 {
			p.suspects[key] /= 2
		} else {
			delete(p.suspects, key)
		}
	}

	delete(p.routeFailedAt, intervalRouteKey(rt))
}

// ReportAttemptFailure folds a failed attempt into both the session's own state
// and the node wide belief.
//
// NOTE: Part of the PaymentResultReporter interface.
func (p *intervalPaymentSession) ReportAttemptFailure(_ uint64, rt *route.Route,
	failureSourceIdx *int, failure lnwire.FailureMessage) {

	if rt == nil || len(rt.Hops) == 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// The HTLC has resolved, so whatever it was holding is no longer held.
	p.releaseRoute(rt)

	p.failedAttempts++

	keys := p.routeKeys(rt)

	// A failure we cannot attribute to any node, or one whose message we
	// could not read, tells us only that something on this route went
	// wrong. That is still worth something, and it is handled by
	// elimination below rather than by penalizing the whole route.
	if failureSourceIdx == nil || failure == nil {
		p.recordUnattributedFailure(rt, keys)

		return
	}

	failIndex := *failureSourceIdx

	// Every hop before the one that failed did forward, which proves it can
	// carry the amount it was handed.
	for i := 0; i < failIndex && i < len(keys); i++ {
		key := keys[i]
		if key.From == p.selfNode {
			continue
		}

		p.store.RecordProbe(
			key, intervalHopAmount(rt, i), p.capacities[key],
		)

		p.decayPenalty(key, intervalProbeDecay)
		if p.suspects[key] > 0 {
			p.suspects[key]--
		}
	}

	// A failure reported by the final node says nothing about the liquidity
	// of any channel, so there is nothing to bound. An index outside the
	// route should not be reachable, but a route we cannot index into is
	// exactly the case where guessing would be worst.
	if failIndex < 0 || failIndex >= len(keys) {
		p.recordRouteFailure(rt, keys)

		return
	}

	key := keys[failIndex]
	amt := intervalHopAmount(rt, failIndex)

	switch failure.Code() {
	// The channel is up but could not carry this amount, which is the one
	// failure that carries a number we can bound with.
	case lnwire.CodeTemporaryChannelFailure:
		if key.From != p.selfNode {
			p.store.RecordFailure(key, amt, p.capacities[key])
		}

		p.recordSessionFailure(key, amt)

	// The policy we hold for this channel is stale, so a retry would be
	// built from the same stale policy. The channel update that came back
	// with the failure has already been applied to the graph by the
	// lifecycle, but this payment has no way to know whether it took, so it
	// steps around the channel for the rest of its life.
	case lnwire.CodeFeeInsufficient, lnwire.CodeIncorrectCltvExpiry:
		p.blocked[key] = struct{}{}
		p.penalties[key] += intervalPolicyPenalty

	default:
		p.blocked[key] = struct{}{}
		p.penalties[key] += intervalUnknownPenalty
	}
}

// recordUnattributedFailure does the attribution work that mission control
// hands to failPairRange, which penalizes every pair on the route because any
// of them could be to blame. Here the suspects are narrowed first: a hop we
// have already proven carries this amount cannot be the one that refused it.
//
// With one suspect left, elimination gives us a certainty for free. With none,
// something we believe is wrong and we say so with a flat penalty. With
// several, the suspicion is shared out and counted, and a channel that keeps
// turning up eventually gets treated as the cause.
func (p *intervalPaymentSession) recordUnattributedFailure(rt *route.Route,
	keys []IntervalKey) {

	p.rejectRoute(rt)

	type suspect struct {
		key IntervalKey
		amt lnwire.MilliSatoshi
	}

	suspects := make([]suspect, 0, len(keys))
	for i, key := range keys {
		if key.From == p.selfNode {
			continue
		}

		// A hop we have already proven carries this amount cannot be
		// the one that refused it.
		amt := intervalHopAmount(rt, i)
		if p.store.Get(key, p.capacities[key]).LowerOK >= amt {
			continue
		}

		suspects = append(suspects, suspect{key: key, amt: amt})
	}

	switch {
	case len(suspects) == 1:
		only := suspects[0]
		p.store.RecordFailure(
			only.key, only.amt, p.capacities[only.key],
		)
		p.recordSessionFailure(only.key, only.amt)

		return

	case len(suspects) == 0:
		for _, key := range keys {
			p.penalties[key] += intervalContradictionPenalty
		}

		return
	}

	share := intervalSuspicionMass / math.Sqrt(float64(len(suspects)))
	for _, item := range suspects {
		p.suspects[item.key]++
		p.penalties[item.key] += share

		if p.suspects[item.key] >= intervalSuspicionEscalation {
			p.penalties[item.key] += intervalExtraSuspicionPenalty
		}
		if p.suspects[item.key] >= intervalSuspicionCertainty {
			p.boundSessionFailure(item.key, item.amt)
		}
	}
}

// recordRouteFailure handles a failure that carries no liquidity information at
// all, such as one reported by the payment's own destination.
func (p *intervalPaymentSession) recordRouteFailure(rt *route.Route,
	keys []IntervalKey) {

	p.rejectRoute(rt)

	for _, key := range keys {
		p.penalties[key] += intervalRoutePenalty
	}
}

// rejectRoute records that this exact route failed at the amount it carried, so
// that it is not offered again at that amount or above.
func (p *intervalPaymentSession) rejectRoute(rt *route.Route) {
	deliver := rt.ReceiverAmt()
	routeKey := intervalRouteKey(rt)

	if previous := p.routeFailedAt[routeKey]; previous == 0 ||
		deliver < previous {

		p.routeFailedAt[routeKey] = deliver
	}
}

// routeRejected reports whether this payment already knows the given route
// cannot carry the given amount.
func (p *intervalPaymentSession) routeRejected(rt *route.Route,
	deliver lnwire.MilliSatoshi) bool {

	failedAt := p.routeFailedAt[intervalRouteKey(rt)]

	return failedAt != 0 && deliver >= failedAt
}

// recordSessionFailure notes that this payment watched a channel refuse an
// amount, and penalizes it for the rest of the payment.
func (p *intervalPaymentSession) recordSessionFailure(key IntervalKey,
	amt lnwire.MilliSatoshi) {

	p.boundSessionFailure(key, amt)
	p.penalties[key] += intervalFailurePenalty
}

// boundSessionFailure lowers this payment's own upper bound for a channel.
func (p *intervalPaymentSession) boundSessionFailure(key IntervalKey,
	amt lnwire.MilliSatoshi) {

	if previous := p.failedAt[key]; previous == 0 || amt < previous {
		p.failedAt[key] = amt
	}
}

// shiftSessionLiquidity moves this payment's own bounds for a channel after a
// part has settled over it, mirroring what the store does to the beliefs.
func (p *intervalPaymentSession) shiftSessionLiquidity(key IntervalKey,
	amt lnwire.MilliSatoshi) {

	if failedAt := p.failedAt[key]; failedAt != 0 {
		if failedAt > amt {
			p.failedAt[key] = failedAt - amt
		} else {
			p.failedAt[key] = 1
		}
	}

	reverse := key.Reverse()
	failedAt, ok := p.failedAt[reverse]
	if !ok || failedAt == 0 {
		return
	}

	// Liquidity that just moved this way is liquidity the other direction
	// gained, so a bound it held may no longer apply at all.
	capacity := p.capacities[key]
	if capacity != 0 && failedAt > capacity-amt {
		delete(p.failedAt, reverse)

		return
	}

	p.failedAt[reverse] = failedAt + amt
}

// decayPenalty softens the penalty on a channel that has just proven itself,
// dropping it entirely once it no longer says anything.
func (p *intervalPaymentSession) decayPenalty(key IntervalKey, factor float64) {
	if penalty := p.penalties[key]; penalty > intervalPenaltyFloor {
		p.penalties[key] = penalty * factor

		return
	}

	delete(p.penalties, key)
}

// recordCapacities remembers the capacity each hop of a route was priced
// against, so that an outcome reported later can be recorded at the same scale.
// Without it we would have to go back to the graph on the failure path to learn
// what a channel's capacity was.
//
// NOTE: a route that did not come from this session, a resumed payment among
// them, leaves no capacity behind. The observations it produces are then
// dropped by the store, which is the right outcome, since an interval means
// nothing without the scale it is measured against.
func (p *intervalPaymentSession) recordCapacities(edges []*unifiedEdge,
	cache *intervalGraphCache) {

	from := p.selfNode
	for _, edge := range edges {
		to := edge.policy.ToNodePubKey()

		key := IntervalKey{
			ChanID: edge.policy.ChannelID,
			From:   from,
			To:     to,
		}

		// Record the hop under the same key it was priced under, so
		// that what we learn from the attempt lands where the next
		// search will look for it.
		scoped := intervalScopeKey(key, cache.siblingCount(from, to))
		if scoped != key {
			p.scopes[key] = scoped
		}

		capacity := lnwire.NewMSatFromSatoshis(edge.capacity)
		if capacity > 0 {
			p.capacities[scoped] = capacity
		}

		from = to
	}
}

// holdRoute records what a route we are about to hand out commits on each of
// its hops, and publishes it to the node wide overlay so that every other
// payment prices those hops knowing about it.
//
// NOTE: the first hop is skipped. The switch already nets our in-flight HTLCs
// out of the bandwidth it reports for our own links, so counting them here as
// well would charge the same liquidity twice.
func (p *intervalPaymentSession) holdRoute(rt *route.Route) {
	amounts := make(map[IntervalKey]lnwire.MilliSatoshi)
	for i, key := range p.routeKeys(rt) {
		if key.From == p.selfNode {
			continue
		}

		amounts[key] += intervalHopAmount(rt, i)
	}

	if len(amounts) == 0 {
		return
	}

	p.outstanding = append(p.outstanding, &heldShard{
		routeKey: intervalRouteKey(rt),
		amounts:  amounts,
	})

	p.store.Hold(amounts)
}

// releaseRoute gives back what one resolved shard was holding. The oldest
// outstanding shard over the same route is the one released, since shards over
// an identical route are indistinguishable and resolve in the order they were
// sent often enough for this to be the better guess.
func (p *intervalPaymentSession) releaseRoute(rt *route.Route) {
	routeKey := intervalRouteKey(rt)

	for i, shard := range p.outstanding {
		if shard.routeKey != routeKey {
			continue
		}

		p.outstanding = append(
			p.outstanding[:i], p.outstanding[i+1:]...,
		)
		p.store.Release(shard.amounts)

		return
	}
}

// reconcileHolds drops the oldest holds until this session is holding no more
// shards than the payment has HTLCs in flight.
//
// The count comes from the payments database by way of the lifecycle, so it is
// ground truth, and this is what makes a hold impossible to leak while a
// payment is running. A route we returned that was never dispatched, because
// the traffic shaper or the database refused it after we handed it over, leaves
// a hold behind that no outcome will ever be reported for. Here it is dropped.
//
// Dropping the oldest is the safe direction. A hold that lingers depresses a
// channel for every payment on the node with nothing behind it, while a hold
// released early only costs us the contention we would have priced in.
func (p *intervalPaymentSession) reconcileHolds(activeShards uint32) {
	for len(p.outstanding) > int(activeShards) {
		stale := p.outstanding[0]
		p.outstanding = p.outstanding[1:]

		p.store.Release(stale.amounts)

		p.log.Debugf("Released a hold on %d channels with no HTLC "+
			"behind it", len(stale.amounts))
	}
}

// ReleaseAttempts gives back everything this session is still holding. The
// payment lifecycle calls it on the way out, which is the last moment anybody
// can, since a session is never reused once its lifecycle has returned.
//
// NOTE: Part of the PaymentResultReporter interface.
func (p *intervalPaymentSession) ReleaseAttempts() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, shard := range p.outstanding {
		p.store.Release(shard.amounts)
	}

	p.outstanding = nil
}

// feeRate returns how this session prices fees, given what the budget has left
// right now.
//
// The two halves of the answer come from different places on purpose. Whether
// there is a budget at all was latched when the session was built, from the
// limit the payment carries. How dearly a budget prices reliability comes from
// the remainder, which the lifecycle recomputes before every request and which
// therefore must never be asked whether a budget exists.
func (p *intervalPaymentSession) feeRate(
	remaining lnwire.MilliSatoshi) intervalFeeRate {

	return newIntervalFeeRate(p.budgeted, remaining)
}

// scoped returns the key a hop of a dispatched route was priced under.
func (p *intervalPaymentSession) scoped(key IntervalKey) IntervalKey {
	if scoped, ok := p.scopes[key]; ok {
		return scoped
	}

	return key
}

// routeKeys returns the key of every hop of a route, at the scope the hop was
// priced under.
func (p *intervalPaymentSession) routeKeys(rt *route.Route) []IntervalKey {
	keys := intervalRouteKeys(rt)
	for i, key := range keys {
		keys[i] = p.scoped(key)
	}

	return keys
}

// intervalRouteKeys returns the directed channel key of every hop of a route.
func intervalRouteKeys(rt *route.Route) []IntervalKey {
	keys := make([]IntervalKey, len(rt.Hops))

	from := rt.SourcePubKey
	for i, hop := range rt.Hops {
		keys[i] = IntervalKey{
			ChanID: hop.ChannelID,
			From:   from,
			To:     hop.PubKeyBytes,
		}
		from = hop.PubKeyBytes
	}

	return keys
}

// intervalHopAmount returns the amount that flows over the given hop of a
// route, which is what the hop before it forwards.
func intervalHopAmount(rt *route.Route, hop int) lnwire.MilliSatoshi {
	if hop == 0 {
		return rt.TotalAmount
	}

	return rt.Hops[hop-1].AmtToForward
}

// intervalRouteKey identifies a route by the channels it walks, so that a route
// that has failed can be recognized when path finding produces it again.
func intervalRouteKey(rt *route.Route) string {
	var key strings.Builder

	fmt.Fprintf(&key, "%x", rt.SourcePubKey[:])
	for _, hop := range rt.Hops {
		fmt.Fprintf(&key, "/%d:%x", hop.ChannelID, hop.PubKeyBytes[:])
	}

	return key.String()
}
