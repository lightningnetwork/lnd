package routing

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// simArrivalWindow keeps at most max_in_flight payments live and starts the
// next one as soon as a slot frees. It is the only arrival process this stage
// implements.
const simArrivalWindow = "window"

// simArrivalPoisson is the arrival process the design spec names and defers.
// An arrival rate interacts with the background traffic prorating carry, which
// is the one piece of the simulator whose draw order every sealed tier depends
// on, so it needs a calibration run of its own rather than a default.
const simArrivalPoisson = "poisson"

// SimConcurrencyParams lets the sender run several of its OWN payments at
// once, which is the one kind of contention the simulator has never had.
//
// Three others already exist and none of them is this one. Sibling shards of a
// single payment contend under atomic_mpp (exp-010b), other people's payments
// move liquidity in the gaps (exp-014), and time passes between attempts. What
// none of them provides is the vantage node racing itself for its own outbound
// liquidity with results interleaved, which is what this section turns on.
//
// Absent, the batch runs one payment at a time, which is what every scenario
// file written before stage D says by omission.
type SimConcurrencyParams struct {
	// MaxInFlight is how many of the sender's payments may be live at
	// once. One is the sequential batch every prior tier ran, and the
	// scheduler is required to reproduce it exactly.
	MaxInFlight int `json:"max_in_flight"`

	// Arrival names the process that starts payments. Empty means
	// "window", the only one implemented: keep MaxInFlight payments live
	// and admit the next as soon as one resolves.
	Arrival string `json:"arrival,omitempty"`

	// InterArrivalSec is how much virtual time passes between a slot
	// freeing and the payment that takes it starting, in seconds. Zero
	// means the clock section's payment_gap_sec, which is the gap the
	// sequential batch has always left between its payments and is what
	// makes max_in_flight=1 reduce to that batch exactly.
	InterArrivalSec float64 `json:"inter_arrival_sec,omitempty"`
}

// validate rejects a concurrency section the scheduler cannot honor, naming
// the deferral where one applies rather than silently running something else.
func (p *SimConcurrencyParams) validate() error {
	if p == nil {
		return nil
	}

	if p.MaxInFlight <= 0 {
		return fmt.Errorf("concurrency: max_in_flight must be "+
			"positive, got %d; omit the section for the "+
			"sequential batch", p.MaxInFlight)
	}

	switch p.Arrival {
	case "", simArrivalWindow:

	case simArrivalPoisson:
		return fmt.Errorf("concurrency: arrival %q is specified but "+
			"DEFERRED; an arrival rate interacts with the "+
			"background traffic prorating carry and needs a "+
			"calibration run of its own, so use %q",
			simArrivalPoisson, simArrivalWindow)

	default:
		return fmt.Errorf("concurrency: unknown arrival %q (want %q)",
			p.Arrival, simArrivalWindow)
	}

	if p.InterArrivalSec < 0 {
		return fmt.Errorf("concurrency: inter_arrival_sec must not "+
			"be negative, got %v", p.InterArrivalSec)
	}

	return nil
}

// maxInFlight returns the window size, treating an absent section as the
// sequential batch.
func (p *SimConcurrencyParams) maxInFlight() int {
	if p == nil {
		return 1
	}

	return p.MaxInFlight
}

// simPaymentState is where a live payment sits inside its own attempt loop.
// The sequential loop this replaces did a whole iteration at a time; the
// scheduler has to be able to stop between the moment a route is chosen and
// the moment its htlc resolves, because that is the only window in which two
// of the sender's payments can be in the air together.
type simPaymentState uint8

const (
	// simPaymentRequest is a payment about to ask its router for the next
	// route to try. Nothing of this payment's is in the air.
	simPaymentRequest simPaymentState = iota

	// simPaymentResolve is a payment whose route has been chosen and whose
	// htlc is on the wire. The step that runs here dispatches it and
	// reports the outcome.
	simPaymentResolve

	// simPaymentDone is a payment that has resolved and released whatever
	// it still held.
	simPaymentDone
)

// simLivePayment is one of the sender's payments, mid-flight. Everything here
// was a local variable of the sequential attempt loop; the scheduler needs it
// to survive between steps.
type simLivePayment struct {
	// index is the payment's position in the scenario list, and the
	// scheduler's tie break. Two payments due at the same virtual instant
	// run in the order the file lists them, always.
	index int

	scenario SimScenario
	result   *SimScenarioResult
	spec     *SimPaymentSpec
	router   SimRouter

	// refresher is the optional half of the contract, non-nil only when
	// this payment's router asked to be told that its own outbound
	// liquidity moved under it.
	refresher SimBalanceRefresher

	state  simPaymentState
	nextAt time.Time

	nextAttemptID uint64
	amtRemaining  lnwire.MilliSatoshi
	inFlightHtlcs uint32

	// holdIDs are the shards that have reached the destination but are not
	// settled yet, only ever populated under atomic mpp.
	holdIDs  []uint64
	heldMsat uint64
	heldFees uint64

	// held is what this payment's own in-flight shards reserve, per
	// directed edge. It is the runner's copy of the graph's holds map,
	// split by payment, which the graph itself does not track.
	held map[simHoldEdge]lnwire.MilliSatoshi

	// pending is the route the request step chose and the resolve step
	// will dispatch, with the attempt id it was given.
	pending   *route.Route
	pendingID uint64
}

// SimConcurrencyStats is what a concurrent batch reports about its own
// scheduling. None of it enters the objective.
//
// MaxConcurrent and MeanConcurrent are the MANIPULATION CHECK, and they are
// the reason this stage ships counters at all: if a file's payments never
// actually overlap then the tier tests nothing, and the scores would look
// perfectly reasonable while measuring the sequential batch under a new name.
// exp-012 shipped a staleness knob without one and spent a whole experiment
// finding out there was no regime there.
//
// SelfContentionFailures is the number the stage exists to produce. Read it
// against MeanConcurrent: a tier that does not overlap cannot contend, and a
// zero here means one of those two things.
type SimConcurrencyStats struct {
	// MaxConcurrent is the largest number of the sender's own payments
	// live at one instant.
	MaxConcurrent int

	// MeanConcurrent is the time-weighted mean number of payments live,
	// over the virtual time in which ANY payment was live. The busy-time
	// denominator is deliberate: over the whole makespan the gaps between
	// payments would drag the mean below one even in a batch that overlaps
	// heavily, and the question this answers is whether payments overlap
	// while they run. It reads exactly 1.0 for a sequential batch.
	MeanConcurrent float64

	// SelfContentionFailures counts the attempts that failed for lack of
	// liquidity on a directed edge where ANOTHER of the sender's own
	// payments was holding some, and would have cleared had that other
	// payment not been holding it. It is causal rather than coincidental:
	// the runner reads the edge's true balance and reservation at the
	// moment of the failure and asks whether the siblings' share of the
	// reservation is what made the difference.
	//
	// It is structurally ZERO without atomic mpp. A shard that settles the
	// instant it arrives reserves nothing, so a batch of non-atomic
	// payments can overlap perfectly and never contend through a hold.
	// That is the stage's free control rather than a defect, and a
	// concurrency tier is expected to set atomic_mpp.
	//
	// It is also computed from the TRUE result rather than the one the
	// router is told. A degraded attribution section damages what came
	// back over the wire, and this is the runner's own book keeping.
	SelfContentionFailures int

	// MakespanSec is the virtual time the batch took to clear, from the
	// scheduler starting to the last payment resolving. It does NOT enter
	// the objective and the program's rule says why: it is a new axis
	// trading against success in an unmeasured way, and this stage changes
	// one thing.
	MakespanSec float64

	// RouterAcceptsBalanceRefresh reports whether the routing strategy
	// under test implements the optional refresh half of the contract, so
	// that "refresh did not help" is distinguishable from "refresh was
	// never delivered". exp-016 had to hand-write importer variants of two
	// champions after the fact because nothing in the contract had ever
	// asked for the capability, and shipping the flag with the capability
	// is what keeps this stage from repeating it.
	RouterAcceptsBalanceRefresh bool
}

// simScheduler is the deterministic virtual-time event loop that replaces the
// sequential attempt loop.
//
// It is NOT goroutines, and the reasons are concrete rather than stylistic.
// SimGraph has no locking on balances or on the holds map. The traffic rng is
// a single stream whose draw order IS the exogenous process. The attribution
// degrader consumes a fixed number of draws per attempt precisely so that two
// routers face the same sequence. Real parallelism would destroy all three,
// and with them the reproducibility every sealed tier depends on.
//
// So the loop is: pick the payment whose next event is earliest, breaking ties
// by scenario index, advance the clock to it, run the background traffic owed
// for the interval, and execute exactly one step of that payment.
type simScheduler struct {
	r         *SimRunner
	source    route.Vertex
	scenarios []SimScenario

	// interArrival is how long a freed slot waits before the next payment
	// takes it, and gapStep by default.
	interArrival time.Duration

	// attemptStep is how much virtual time one htlc attempt consumes.
	attemptStep time.Duration

	// inFlight is the window size, one for the sequential batch.
	inFlight int

	results []*SimScenarioResult
	live    []*simLivePayment

	// next is the index of the next scenario waiting for a slot.
	next int

	// slotFree holds the instants at which the free slots became free, in
	// ascending order. A payment is admitted interArrival after the
	// EARLIEST of them, which is what makes max_in_flight=1 reproduce the
	// sequential batch: there the single slot frees when the previous
	// payment resolved, and the gap is measured from exactly there.
	slotFree []time.Time

	start       time.Time
	lastAccount time.Time
	lastFinish  time.Time

	liveIntegral time.Duration
	busy         time.Duration

	stats SimConcurrencyStats
}

// newSimScheduler builds the loop for one batch.
func newSimScheduler(r *SimRunner, source route.Vertex,
	scenarios []SimScenario,
	params *SimConcurrencyParams) (*simScheduler, error) {

	if err := params.validate(); err != nil {
		return nil, err
	}

	inFlight := params.maxInFlight()
	interArrival := r.simGapStep()
	if params != nil && params.InterArrivalSec > 0 {
		interArrival = time.Duration(
			params.InterArrivalSec * float64(time.Second),
		)
	}

	// Concurrency without a clock is not concurrency. Every event ties at
	// the zero instant, so the loop degenerates to running the payments in
	// index order with nothing ever overlapping, and the scheduling
	// counters would report a window that never opened. A tier asking for
	// one gets told rather than measured.
	if inFlight > 1 && r.simAttemptStep() == 0 {
		return nil, fmt.Errorf("concurrency: max_in_flight %d needs a "+
			"clock section with a positive attempt_sec; with no "+
			"virtual time payments cannot overlap", inFlight)
	}

	return &simScheduler{
		r:            r,
		source:       source,
		scenarios:    scenarios,
		interArrival: interArrival,
		attemptStep:  r.simAttemptStep(),
		inFlight:     inFlight,
	}, nil
}

// run executes the batch and returns the results in scenario order. On a fatal
// error it names the scenario that produced it, so that the caller can report
// it the way the sequential batch always has.
func (s *simScheduler) run() ([]*SimScenarioResult, int, error) {
	if s.r.graph.Node(s.source) == nil {
		return nil, 0, fmt.Errorf("source node %v not in graph",
			s.source)
	}

	s.start = s.r.simSchedTime()
	s.lastAccount = s.start
	s.lastFinish = s.start
	s.results = make([]*SimScenarioResult, len(s.scenarios))

	s.slotFree = make([]time.Time, s.inFlight)
	for i := range s.slotFree {
		s.slotFree[i] = s.start
	}

	for {
		admitAt, hasAdmit := s.nextAdmission()
		payAt, pay := s.nextEvent()

		switch {
		case !hasAdmit && pay == nil:
			s.finalize()

			return s.results, 0, nil

		// An admission and a payment event due at the same instant fill
		// the window first, which is the only reading of "keep
		// max_in_flight payments live" that does not leave a slot idle
		// while a payment that could have started waits.
		case hasAdmit && (pay == nil || !payAt.Before(admitAt)):
			s.advanceTo(admitAt)

			idx := s.next
			if err := s.admit(); err != nil {
				s.abandon()

				return nil, idx, err
			}

		default:
			if err := s.stepAt(payAt, pay); err != nil {
				s.abandon()

				return nil, pay.index, err
			}
		}
	}
}

// nextAdmission returns when the next waiting payment starts, if one is
// waiting and a slot is free for it.
func (s *simScheduler) nextAdmission() (time.Time, bool) {
	if s.next >= len(s.scenarios) || len(s.slotFree) == 0 {
		return time.Time{}, false
	}

	return s.slotFree[0].Add(s.interArrival), true
}

// nextEvent returns the live payment whose next step is due earliest, ties
// broken by scenario index.
func (s *simScheduler) nextEvent() (time.Time, *simLivePayment) {
	var best *simLivePayment
	for _, p := range s.live {
		switch {
		case best == nil:
		case p.nextAt.Before(best.nextAt):
		case p.nextAt.Equal(best.nextAt) && p.index < best.index:
		default:
			continue
		}

		best = p
	}

	if best == nil {
		return time.Time{}, nil
	}

	return best.nextAt, best
}

// advanceTo moves the clock to an instant and charges the interval to the
// concurrency accounting.
//
// Whether the background traffic runs for the interval is the sequential
// loop's own rule, generalized: the gap between payments always churned, and
// the time inside a payment churned only under atomic mpp. With one payment
// live those two cases are exactly "no payment live" and "the live payment is
// atomic", and that is what this asks.
func (s *simScheduler) advanceTo(target time.Time) {
	s.r.simAdvanceTo(target, s.trafficRuns())
	s.accountTo(s.r.simSchedTime())
}

// trafficRuns reports whether the exogenous process should run over the
// interval about to elapse.
func (s *simScheduler) trafficRuns() bool {
	if len(s.live) == 0 {
		return true
	}

	for _, p := range s.live {
		if p.scenario.AtomicMpp {
			return true
		}
	}

	return false
}

// accountTo charges the stretch since the last accounting point to the
// concurrency integral, at the live count that held over it. Every caller that
// is about to change the live set calls it first, so the integral is exact
// rather than sampled.
func (s *simScheduler) accountTo(now time.Time) {
	d := now.Sub(s.lastAccount)
	if d <= 0 {
		return
	}

	s.liveIntegral += time.Duration(len(s.live)) * d
	if len(s.live) > 0 {
		s.busy += d
	}
	s.lastAccount = now
}

// admit starts the next waiting payment. Everything it does before the first
// route request is what the sequential loop did in the same order: the gap's
// churn, then the target, then the budget, then the router, then any served
// observations still waiting for one.
func (s *simScheduler) admit() error {
	// The payment gap's churn happens whether or not any virtual time
	// passes. That is what the sequential loop did unconditionally, so a
	// scenario file with background traffic and no clock still churns once
	// per payment; the prorating path has no duration to work with there,
	// so this is the case that keeps it.
	if s.r.virtualClk == nil || s.r.clockParams.PaymentGapSec <= 0 {
		if s.r.traffic != nil {
			s.r.traffic.run()
		}
	}

	now := s.r.simSchedTime()
	s.accountTo(now)

	idx := s.next
	s.next++
	s.slotFree = s.slotFree[1:]

	scenario := s.scenarios[idx]
	result := &SimScenarioResult{Scenario: scenario}
	s.results[idx] = result

	target, err := s.r.graph.ResolveNode(scenario.Target)
	if err != nil {
		return err
	}

	maxParts := scenario.MaxParts
	if maxParts == 0 {
		maxParts = 16
	}

	amount := lnwire.MilliSatoshi(scenario.AmtMsat)
	spec := &SimPaymentSpec{
		Target:   target,
		Amount:   amount,
		MaxParts: maxParts,

		// The budget is quoted as a share of the payment's own amount,
		// so that one number describes a corpus whose amounts run over
		// four orders of magnitude. With no limit set this is
		// lnwire.MaxMilliSatoshi, which is the value the lnd arm has
		// been constructed with for the whole program.
		FeeLimitMsat: simFeeBudgetMsat(amount, scenario.FeeLimitPPM),
	}

	if spec.FeeLimitMsat != lnwire.MaxMilliSatoshi {
		s.r.feeLimitStats.Payments++
	}

	// Build the routing strategy under test for this payment, handing it
	// the public graph view and the sender's exact local balances. The view
	// wrapper hides the concrete graph so that a candidate router cannot
	// reach the hidden balances.
	//
	// LocalBalances is read HERE, which is what makes concurrency bite
	// without any new plumbing: it returns each end's available liquidity,
	// net of what the sender's other in-flight shards already hold, so a
	// payment starting while a sibling holds sees the reduced balance.
	router, err := s.r.routerFactory(
		&simGossipView{g: s.r.graph, now: s.r.clk.Now}, s.source,
		s.r.graph.LocalBalances(s.source), spec,
	)
	if err != nil {
		return err
	}

	// Hand over any served knowledge before the router plans anything, so
	// that imported beliefs are available to the very first route request
	// rather than arriving after the payment has committed.
	if err := s.r.deliverPendingImport(router); err != nil {
		return err
	}

	p := &simLivePayment{
		index:        idx,
		scenario:     scenario,
		result:       result,
		spec:         spec,
		router:       router,
		state:        simPaymentRequest,
		nextAt:       now,
		amtRemaining: amount,
		held:         make(map[simHoldEdge]lnwire.MilliSatoshi),
	}

	refresher, accepts := router.(SimBalanceRefresher)
	if accepts {
		p.refresher = refresher
	}
	s.r.noteBalanceRefreshCapability(accepts)

	s.live = append(s.live, p)
	if len(s.live) > s.stats.MaxConcurrent {
		s.stats.MaxConcurrent = len(s.live)
	}

	return nil
}

// stepAt advances the clock to a payment's next event and runs exactly one
// step of it: one RequestRoute, or one dispatch and one ReportAttempt.
func (s *simScheduler) stepAt(at time.Time, p *simLivePayment) error {
	s.advanceTo(at)

	switch p.state {
	case simPaymentRequest:
		return s.stepRequest(p)

	case simPaymentResolve:
		return s.stepResolve(p)
	}

	return nil
}

// stepRequest asks the router for the next route to try, prices it against the
// payment's budget, and puts the htlc in the air.
func (s *simScheduler) stepRequest(p *simLivePayment) error {
	// The attempt cap is checked here because here is the top of the
	// sequential loop: a degenerate router cannot spin forever.
	if len(p.result.Attempts) >= simMaxAttempts {
		s.finish(p)

		return nil
	}

	// Tell the router that its own outbound liquidity moved under it, if it
	// asked to be told. Every router in this program keeps the map it was
	// handed at construction and nothing updates it, which is fine when one
	// payment runs at a time and wrong the moment two do.
	if p.refresher != nil {
		p.refresher.RefreshLocalBalances(
			s.r.graph.LocalBalances(s.source),
		)
	}

	rt, err := p.router.RequestRoute(p.amtRemaining, p.inFlightHtlcs)
	if err != nil {
		p.result.Error = err.Error()
		p.result.GaveUp = true
		s.finish(p)

		return nil
	}

	attemptID := p.nextAttemptID
	p.nextAttemptID++

	// The fee budget is enforced HERE, at the point the runner would
	// dispatch, and not inside any router. That is the exp-019
	// construction: a constraint that lives at the shared delivery point is
	// the same constraint for the lnd stack and for an evolved candidate.
	//
	// What the budget has left is what it started with less the fees this
	// payment has already committed, which is the fees of the shards that
	// settled plus the fees riding on the ones still held.
	committed := lnwire.MilliSatoshi(p.result.FeeMsat + p.heldFees)
	remaining := simRemainingBudget(p.spec.FeeLimitMsat, committed)
	if rt.TotalFees() > remaining {
		// The refusal is a fact about this sender, not about the
		// network: no forwarding node saw the htlc, so nothing is
		// recorded in the observation stream, no virtual time passes,
		// and the result is handed to the router undegraded.
		//
		// It does cost an attempt. A router that keeps offering routes
		// it cannot afford spends its attempt budget on them, which is
		// the whole point of putting the pressure in the environment.
		refusal := SimHtlcResult{
			FailureSource: rt.SourcePubKey,
			Failure:       SimFeeLimitFailure{},
		}

		p.result.Attempts = append(
			p.result.Attempts, traceAttempt(rt, refusal),
		)
		s.r.feeLimitStats.Failures++

		return p.router.ReportAttempt(attemptID, rt, refusal)
	}

	// The htlc is now in the air and resolves one attempt's worth of
	// virtual time from now, which is the window another of the sender's
	// payments can run inside.
	p.pending = rt
	p.pendingID = attemptID
	p.state = simPaymentResolve
	p.nextAt = s.r.simSchedTime().Add(s.attemptStep)

	return nil
}

// stepResolve sends the pending htlc through the simulated network and reports
// what came back.
func (s *simScheduler) stepResolve(p *simLivePayment) error {
	rt := p.pending
	p.pending = nil
	p.state = simPaymentRequest

	// A malformed route (unknown channel, disconnected hops) is a router
	// bug: it terminates this payment with an error rather than killing the
	// whole batch, so one bad edge case doesn't zero out an otherwise
	// functional candidate.
	//
	// An atomic shard is held at the destination rather than settled there,
	// reserving the liquidity of every hop it crossed until the payment as
	// a whole resolves.
	var (
		htlcResult SimHtlcResult
		holdID     uint64
		err        error
	)
	if p.scenario.AtomicMpp {
		htlcResult, holdID, err = s.r.graph.HoldHtlc(rt)
	} else {
		htlcResult, err = s.r.graph.SendHtlc(rt)
	}
	if err != nil {
		p.result.Error = fmt.Sprintf("malformed route: %v", err)
		s.finish(p)

		return nil
	}

	p.result.Attempts = append(
		p.result.Attempts, traceAttempt(rt, htlcResult),
	)

	s.noteSelfContention(p, rt, htlcResult)

	// Record what this attempt revealed about the edges it crossed, which
	// is the raw material a weight-serving node would have to offer.
	s.r.observations = append(s.r.observations, observationsFromAttempt(
		rt, htlcResult, s.r.clk.Now(),
	)...)

	// Let the router learn from the outcome. Everything above this line
	// records what actually happened; what the router is TOLD may be less
	// than that, since an attribution section ages and damages the result
	// on its way over.
	err = p.router.ReportAttempt(
		p.pendingID, rt, s.r.deliverAttempt(rt, htlcResult),
	)
	if err != nil {
		return err
	}

	// deliverAttempt can age the network by whole attempt-sized slices, so
	// the next request is due at whatever the clock says now.
	p.nextAt = s.r.simSchedTime()

	if htlcResult.Failure != nil {
		return nil
	}

	p.inFlightHtlcs++

	// A settling shard pays its fee right away; a held one only pays when
	// the whole set settles.
	if p.scenario.AtomicMpp {
		p.holdIDs = append(p.holdIDs, holdID)
		p.heldMsat += uint64(rt.ReceiverAmt())
		p.heldFees += uint64(rt.TotalFees())

		for _, res := range s.r.graph.holdReservations(holdID) {
			p.held[res.edge] += res.amt
		}
	} else {
		p.result.FeeMsat += uint64(rt.TotalFees())
	}

	// Guard against a buggy router delivering more than asked: unsigned
	// underflow here would loop until the attempt cap.
	recv := rt.ReceiverAmt()
	if recv > p.amtRemaining {
		p.result.Error = "router over-delivered payment amount"
		s.finish(p)

		return nil
	}
	p.amtRemaining -= recv

	if p.amtRemaining == 0 {
		// The full amount has arrived, so the held set becomes real
		// balance movement all at once and the fees it carried finally
		// come due. Without atomic mpp there is nothing held and this is
		// a no-op.
		for _, id := range p.holdIDs {
			s.r.graph.SettleHold(id)
		}
		p.holdIDs = nil
		p.result.FeeMsat += p.heldFees

		p.result.Success = true
		s.finish(p)
	}

	return nil
}

// noteSelfContention attributes an attempt that failed for want of liquidity
// to the sender's OWN other payments, when they are what made the difference.
//
// The test is causal rather than coincidental. The runner reads the failing
// edge's true balance and its total reservation at this instant, and asks
// whether the edge would have carried the amount with the siblings' share of
// that reservation removed. An edge that was short anyway is not contention,
// and an edge that had room to spare did not fail for liquidity at all.
func (s *simScheduler) noteSelfContention(p *simLivePayment, rt *route.Route,
	res SimHtlcResult) {

	if res.Failure == nil {
		return
	}

	// A liquidity shortfall is the temporary channel failure the forwarding
	// check returns. The announced-limit refusals of stage A share the code
	// and are filtered out below by the balance test: an edge with room to
	// spare cannot have failed on liquidity.
	if _, ok := res.Failure.(*lnwire.FailTemporaryChannelFailure); !ok {
		return
	}

	idx := getNodeIndexSim(rt, res.FailureSource)
	if idx == nil || *idx >= len(rt.Hops) {
		return
	}

	edge := simHoldEdge{
		ChanID: rt.Hops[*idx].ChannelID,
		From:   res.FailureSource,
	}

	// The amount the failing hop was asked to send is the route total at
	// the first channel and the previous hop's amt-to-forward after that,
	// matching walkHtlc's own accounting.
	amtOut := rt.TotalAmount
	if *idx > 0 {
		amtOut = rt.Hops[*idx-1].AmtToForward
	}

	var siblings lnwire.MilliSatoshi
	for _, q := range s.live {
		if q == p {
			continue
		}

		siblings += q.held[edge]
	}
	if siblings == 0 {
		return
	}

	balance, held, ok := s.r.graph.endLiquidity(edge.ChanID, edge.From)
	if !ok || held > balance {
		return
	}

	available := balance - held
	if available >= amtOut {
		return
	}
	if available+siblings < amtOut {
		return
	}

	s.stats.SelfContentionFailures++
}

// finish resolves a payment: it releases whatever the payment still held,
// takes it out of the live set and frees its slot.
func (s *simScheduler) finish(p *simLivePayment) {
	now := s.r.simSchedTime()
	s.accountTo(now)

	// Under atomic mpp a payment that never completes settles nothing:
	// every shard still held gives its reserved liquidity back, so a failed
	// mpp leaves the hidden balances exactly as it found them and charges
	// no fees. The success path settles the set and clears holdIDs first,
	// so this only ever fires on a failure path, whichever one it is.
	if len(p.holdIDs) > 0 {
		for _, id := range p.holdIDs {
			s.r.graph.ReleaseHold(id)
		}
		p.result.HeldReleasedMsat = p.heldMsat
	}
	p.holdIDs = nil
	p.held = nil
	p.state = simPaymentDone

	for i, q := range s.live {
		if q != p {
			continue
		}

		s.live = append(s.live[:i], s.live[i+1:]...)

		break
	}

	// Virtual time never runs backwards, so the freed slot belongs at the
	// end of the ascending list.
	s.slotFree = append(s.slotFree, now)
	s.lastFinish = now
}

// abandon releases everything the still-live payments hold, which is what the
// sequential loop's deferred release did on the error paths that killed a
// batch. Without it a fatal error would leave the graph reserving liquidity
// for htlcs that will never resolve.
func (s *simScheduler) abandon() {
	for _, p := range s.live {
		for _, id := range p.holdIDs {
			s.r.graph.ReleaseHold(id)
		}
		p.holdIDs = nil
	}
	s.live = nil
}

// finalize closes the accounting and computes the reported ratios.
func (s *simScheduler) finalize() {
	s.accountTo(s.r.simSchedTime())

	if s.busy > 0 {
		s.stats.MeanConcurrent = s.liveIntegral.Seconds() /
			s.busy.Seconds()
	}
	s.stats.MakespanSec = s.lastFinish.Sub(s.start).Seconds()
	s.stats.RouterAcceptsBalanceRefresh = s.r.RouterAcceptsBalanceRefresh()
}
