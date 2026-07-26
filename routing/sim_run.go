package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// simMaxAttempts caps the number of htlc attempts per payment so that a
// degenerate parameter set cannot loop forever.
const simMaxAttempts = 200

// SimParams is the candidate under optimization: every knob of the path
// finding heuristic that the optimizer may tune, in a JSON-friendly shape.
type SimParams struct {
	// Estimator selects the probability model: "apriori" or "bimodal".
	Estimator string `json:"estimator"`

	// Apriori holds the apriori estimator parameters, used when
	// Estimator is "apriori".
	Apriori SimAprioriParams `json:"apriori"`

	// Bimodal holds the bimodal estimator parameters, used when
	// Estimator is "bimodal".
	Bimodal SimBimodalParams `json:"bimodal"`

	// AttemptCostMsat is the virtual fixed cost of a payment attempt.
	AttemptCostMsat int64 `json:"attempt_cost_msat"`

	// AttemptCostPPM is the virtual proportional cost of a payment
	// attempt, in parts per million of the payment amount.
	AttemptCostPPM int64 `json:"attempt_cost_ppm"`

	// MinProbability is the minimum success probability a candidate
	// route must have to be attempted.
	MinProbability float64 `json:"min_probability"`
}

// SimAprioriParams mirrors AprioriConfig in JSON-friendly units.
type SimAprioriParams struct {
	PenaltyHalfLifeSec float64 `json:"penalty_half_life_sec"`
	HopProbability     float64 `json:"hop_probability"`
	Weight             float64 `json:"weight"`
	CapacityFraction   float64 `json:"capacity_fraction"`
}

// SimBimodalParams mirrors BimodalConfig in JSON-friendly units.
type SimBimodalParams struct {
	ScaleMsat    uint64  `json:"scale_msat"`
	NodeWeight   float64 `json:"node_weight"`
	DecayTimeSec float64 `json:"decay_time_sec"`
}

// DefaultSimParams returns the current lnd defaults as a SimParams, the
// natural seed candidate for optimization.
func DefaultSimParams() *SimParams {
	return &SimParams{
		Estimator: "apriori",
		Apriori: SimAprioriParams{
			PenaltyHalfLifeSec: DefaultPenaltyHalfLife.Seconds(),
			HopProbability:     DefaultAprioriHopProbability,
			Weight:             DefaultAprioriWeight,
			CapacityFraction:   DefaultCapacityFraction,
		},
		Bimodal: SimBimodalParams{
			ScaleMsat:  uint64(DefaultBimodalScaleMsat),
			NodeWeight: DefaultBimodalNodeWeight,
			DecayTimeSec: DefaultBimodalDecayTime.
				Seconds(),
		},
		AttemptCostMsat: int64(DefaultAttemptCost),
		AttemptCostPPM:  DefaultAttemptCostPPM,
		MinProbability:  DefaultMinRouteProbability,
	}
}

// buildEstimator instantiates the configured probability estimator,
// validating parameter ranges.
func (p *SimParams) buildEstimator() (Estimator, error) {
	switch p.Estimator {
	case "apriori":
		cfg := AprioriConfig{
			PenaltyHalfLife: time.Duration(
				p.Apriori.PenaltyHalfLifeSec *
					float64(time.Second),
			),
			AprioriHopProbability: p.Apriori.HopProbability,
			AprioriWeight:         p.Apriori.Weight,
			CapacityFraction:      p.Apriori.CapacityFraction,
		}
		return NewAprioriEstimator(cfg)

	case "bimodal":
		cfg := BimodalConfig{
			BimodalScaleMsat: lnwire.MilliSatoshi(
				p.Bimodal.ScaleMsat,
			),
			BimodalNodeWeight: p.Bimodal.NodeWeight,
			BimodalDecayTime: time.Duration(
				p.Bimodal.DecayTimeSec *
					float64(time.Second),
			),
		}
		return NewBimodalEstimator(cfg)

	default:
		return nil, fmt.Errorf("unknown estimator %q", p.Estimator)
	}
}

// pathFindingConfig converts the params to a PathFindingConfig.
func (p *SimParams) pathFindingConfig() PathFindingConfig {
	return PathFindingConfig{
		AttemptCost: lnwire.MilliSatoshi(
			p.AttemptCostMsat,
		),
		AttemptCostPPM: p.AttemptCostPPM,
		MinProbability: p.MinProbability,
	}
}

// SimScenario is a single payment to attempt in the simulated network. All
// scenarios of a runner share the same source node, whose mission control
// accumulates knowledge across scenarios just like a real node's would.
type SimScenario struct {
	// Target is a node reference: a 66-char hex pubkey, an alias, or a
	// numeric id for synthetic topologies.
	Target string `json:"target"`

	// AmtMsat is the payment amount.
	AmtMsat uint64 `json:"amt_msat"`

	// MaxParts caps the number of MPP shards. 1 disables splitting.
	MaxParts uint32 `json:"max_parts"`

	// AtomicMpp switches the payment onto hold-and-release shard
	// semantics: a shard that reaches the destination reserves the
	// liquidity of every hop it crossed instead of settling it, and the
	// whole set only moves balances once the full amount has arrived. A
	// payment that never completes releases everything it held, so a
	// failed mpp is atomic and costs no fees. It also makes sequential
	// probing expensive rather than free: the shards a router leaves in
	// flight while it probes contend with its own siblings and with
	// background traffic, and time keeps passing between attempts.
	//
	// With the flag off the simulator keeps its historical behavior, in
	// which every shard settles the instant it arrives.
	AtomicMpp bool `json:"atomic_mpp,omitempty"`
}

// SimHopTrace records one hop of an attempted route.
type SimHopTrace struct {
	ChanID  uint64 `json:"chan_id"`
	PubKey  string `json:"pub_key"`
	AmtMsat uint64 `json:"amt_msat"`
}

// SimAttemptTrace records one htlc attempt and its outcome, the raw feedback
// the optimizer's reflection step consumes.
type SimAttemptTrace struct {
	Hops       []SimHopTrace `json:"hops"`
	AmtMsat    uint64        `json:"amt_msat"`
	FeeMsat    uint64        `json:"fee_msat"`
	Success    bool          `json:"success"`
	FailureIdx int           `json:"failure_hop,omitempty"`
	Failure    string        `json:"failure,omitempty"`
}

// SimScenarioResult is the outcome of one scenario.
type SimScenarioResult struct {
	Scenario SimScenario       `json:"scenario"`
	Success  bool              `json:"success"`
	Attempts []SimAttemptTrace `json:"attempts"`

	// FeeMsat is the total fee paid over all settled htlcs.
	FeeMsat uint64 `json:"fee_msat"`

	// HeldReleasedMsat is how much of the payment had already reached the
	// destination and was rolled back when the payment failed. It is only
	// ever non-zero under atomic mpp, where it measures the liquidity a
	// router tied up on the way to failing.
	HeldReleasedMsat uint64 `json:"held_released_msat,omitempty"`

	// Error records a terminal payment error, e.g. no path found.
	Error string `json:"error,omitempty"`
}

// SimClockParams configures the virtual clock. Without one the simulation
// runs on the wall clock, where a whole batch finishes in well under any
// decay half-life; with one, simulated time passes between payments and
// attempts so that time-based logic (mission control decay, candidate
// staleness handling) actually operates.
type SimClockParams struct {
	// StartUnix anchors the virtual clock; a fixed value keeps runs
	// reproducible. Zero selects a fixed default epoch.
	StartUnix int64 `json:"start_unix"`

	// PaymentGapSec is how much virtual time passes before each scenario
	// payment, the window in which background traffic moves liquidity.
	PaymentGapSec float64 `json:"payment_gap_sec"`

	// AttemptSec is how much virtual time each htlc attempt consumes.
	AttemptSec float64 `json:"attempt_sec"`
}

// simDefaultClockStart is the fixed virtual epoch used when a clock section
// doesn't pin one, chosen arbitrarily but deterministically.
const simDefaultClockStart int64 = 1_750_000_000

// SimRunner runs payment scenarios against a simulated network with a
// persistent mission control, mirroring the control loop of a real node.
type SimRunner struct {
	graph  *SimGraph
	source route.Vertex
	mc     *MissionControl
	mcc    *MissionController
	params *SimParams

	// routerFactory builds the routing strategy under test, once per
	// payment. Defaults to the lnd production stack.
	routerFactory SimRouterFactory

	// clk is the time source routers observe. It is the wall clock
	// unless a virtual clock is configured.
	clk clock.Clock

	// virtualClk is the settable clock behind clk when virtual time is
	// enabled, nil otherwise.
	virtualClk *clock.TestClock

	// clockParams holds the virtual time step sizes.
	clockParams SimClockParams

	// traffic is the background traffic engine, nil when disabled.
	traffic *simTraffic

	// trafficCarry is the fractional background payment left over from
	// pro-rating the per-gap volume across attempts. Carrying it keeps the
	// traffic rate inside a payment equal to the rate between payments
	// instead of rounding it away.
	trafficCarry float64

	cleanup func()
}

// NewSimRunner creates a runner that pays from the given source node with
// the given parameter set. The mission control state is backed by a
// throwaway bolt db in dir.
func NewSimRunner(graph *SimGraph, params *SimParams, source route.Vertex,
	dir string) (*SimRunner, error) {

	if graph.Node(source) == nil {
		return nil, fmt.Errorf("source node %v not in graph", source)
	}

	estimator, err := params.buildEstimator()
	if err != nil {
		return nil, err
	}

	dbDir, err := os.MkdirTemp(dir, "routesim-mc-*")
	if err != nil {
		return nil, err
	}

	db, err := kvdb.Create(
		kvdb.BoltBackendName, filepath.Join(dbDir, "mc.db"), true,
		kvdb.DefaultDBTimeout, false,
	)
	if err != nil {
		os.RemoveAll(dbDir)
		return nil, err
	}

	cleanup := func() {
		db.Close()
		os.RemoveAll(dbDir)
	}

	// Mission control is anchored to the source node so that local
	// channels get the distinct local probability estimate, just like on
	// a real node.
	mcCfg := &MissionControlConfig{Estimator: estimator}
	mcController, err := NewMissionController(db, source, mcCfg)
	if err != nil {
		cleanup()
		return nil, err
	}

	mc, err := mcController.GetNamespacedStore(
		DefaultMissionControlNamespace,
	)
	if err != nil {
		cleanup()
		return nil, err
	}

	runner := &SimRunner{
		graph:   graph,
		source:  source,
		mc:      mc,
		mcc:     mcController,
		params:  params,
		clk:     clock.NewDefaultClock(),
		cleanup: cleanup,
	}

	// The default routing strategy is lnd's production stack; candidate
	// algorithms replace it via SetRouterFactory.
	runner.routerFactory = func(view SimNetworkView, src route.Vertex,
		localBalances map[uint64]lnwire.MilliSatoshi,
		spec *SimPaymentSpec) (SimRouter, error) {

		return newLndStackRouter(
			view, mc, params, src, localBalances, spec,
		)
	}

	return runner, nil
}

// SetRouterFactory replaces the routing strategy under test.
func (r *SimRunner) SetRouterFactory(factory SimRouterFactory) {
	r.routerFactory = factory
}

// SetVirtualClock switches the runner (and the mission control stack behind
// the lnd baseline) onto a settable virtual clock, so that decay half-lives
// and other time-based logic operate over simulated time rather than the
// microseconds a batch takes on the wall clock.
func (r *SimRunner) SetVirtualClock(params *SimClockParams) {
	start := params.StartUnix
	if start == 0 {
		start = simDefaultClockStart
	}

	r.clockParams = *params
	r.virtualClk = clock.NewTestClock(time.Unix(start, 0))
	r.clk = r.virtualClk

	// Mission control instances share the controller's config, so
	// swapping the clock here reaches every namespace.
	r.mcc.cfg.clock = r.virtualClk
}

// SetBackgroundTraffic enables the exogenous traffic model: before each
// scenario payment, the configured number of seeded background payments
// execute between random node pairs, moving hidden liquidity the way other
// people's payments do on a live network.
func (r *SimRunner) SetBackgroundTraffic(params *SimTrafficParams) error {
	traffic, err := newSimTraffic(r.graph, params)
	if err != nil {
		return err
	}
	r.traffic = traffic

	return nil
}

// TrafficStats reports how many background payments were sent and settled.
func (r *SimRunner) TrafficStats() (sent, settled int) {
	if r.traffic == nil {
		return 0, 0
	}

	return r.traffic.Sent, r.traffic.Settled
}

// AdvanceIdle moves virtual time forward by the given number of seconds
// without sending a scenario payment of its own, letting the background
// traffic use that window at its usual rate. It models the gap between the
// moment routing knowledge was gathered and the moment it is finally used:
// the sender learns nothing new while other people's payments keep moving
// hidden liquidity, so whatever it believes about the network ages. With no
// virtual clock the time half is a no-op, and with no traffic model the
// liquidity half is, which makes an idle stretch on a static simulation cost
// exactly nothing. The traffic rate comes from the payment gap, so a traffic
// model configured without one has no defined rate and moves nothing here.
func (r *SimRunner) AdvanceIdle(seconds float64) {
	if seconds <= 0 {
		return
	}

	if r.virtualClk != nil {
		r.virtualClk.SetTime(r.virtualClk.Now().Add(
			time.Duration(seconds * float64(time.Second)),
		))
	}

	if r.traffic != nil {
		r.traffic.runN(r.trafficPaymentsFor(seconds))
	}
}

// advanceGap moves virtual time forward by the payment gap and lets the
// background traffic use that window.
func (r *SimRunner) advanceGap() {
	if r.virtualClk != nil && r.clockParams.PaymentGapSec > 0 {
		r.virtualClk.SetTime(r.virtualClk.Now().Add(
			time.Duration(r.clockParams.PaymentGapSec *
				float64(time.Second)),
		))
	}

	if r.traffic != nil {
		r.traffic.run()
	}
}

// advanceAttempt moves virtual time forward by one attempt's duration. Under
// atomic mpp the background traffic engine also runs for that slice of time,
// so hidden liquidity keeps drifting while a payment's shards are in flight
// rather than freezing until the payment resolves. That is what makes a
// serial probe-learn-resize strategy pay for the time it takes.
func (r *SimRunner) advanceAttempt(atomicMpp bool) {
	if r.virtualClk == nil || r.clockParams.AttemptSec <= 0 {
		return
	}

	r.virtualClk.SetTime(r.virtualClk.Now().Add(
		time.Duration(r.clockParams.AttemptSec *
			float64(time.Second)),
	))

	if !atomicMpp || r.traffic == nil {
		return
	}

	r.traffic.runN(r.trafficPaymentsFor(r.clockParams.AttemptSec))
}

// trafficPaymentsFor returns how many background payments belong to the given
// stretch of virtual time. The per-gap volume is pro-rated by that duration so
// that the exogenous process runs at one rate throughout, and the fractional
// remainder carries into the next stretch rather than rounding away.
func (r *SimRunner) trafficPaymentsFor(seconds float64) int {
	if r.traffic == nil || r.clockParams.PaymentGapSec <= 0 {
		return 0
	}

	r.trafficCarry += float64(r.traffic.params.PaymentsPerGap) *
		seconds / r.clockParams.PaymentGapSec

	n := int(r.trafficCarry)
	r.trafficCarry -= float64(n)

	return n
}

// Close releases the runner's resources.
func (r *SimRunner) Close() {
	r.cleanup()
}

// ResetHistory wipes the mission control learning state.
func (r *SimRunner) ResetHistory() error {
	return r.mc.ResetHistory()
}

// ResolveNode resolves a node reference: hex pubkey, numeric synthetic id,
// or alias.
func (g *SimGraph) ResolveNode(ref string) (route.Vertex, error) {
	// Try a hex pubkey first.
	if len(ref) == 66 {
		if v, err := route.NewVertexFromStr(ref); err == nil {
			if g.Node(v) != nil {
				return v, nil
			}
			return route.Vertex{}, fmt.Errorf("node %v not in "+
				"graph", ref)
		}
	}

	// Then a numeric synthetic id.
	var id uint32
	if _, err := fmt.Sscanf(ref, "%d", &id); err == nil {
		v := SimNodePubKey(id)
		if g.Node(v) != nil {
			return v, nil
		}
	}

	// Finally an alias.
	return g.NodeByAlias(ref)
}

// ResolveNode resolves a node reference against the runner's graph: hex
// pubkey, numeric synthetic id, or alias.
func (r *SimRunner) ResolveNode(ref string) (route.Vertex, error) {
	return r.graph.ResolveNode(ref)
}

// RunScenario executes a single payment scenario and returns its trace. The
// mission control state carries over between scenarios, so ordering matters,
// just like consecutive payments on a real node.
func (r *SimRunner) RunScenario(s *SimScenario) (*SimScenarioResult, error) {
	return r.RunScenarioFrom(r.source, s)
}

// RunScenarioFrom executes a payment scenario sent by the given node instead
// of the runner's own source, and is otherwise identical to RunScenario: the
// same graph, the same clock, the same background traffic, and the same
// mission control.
//
// A foreign sender is how the simulation models knowledge that somebody else
// gathered: the payment probes real channels and everything it learns lands in
// the one shared mission control, which stays anchored to the runner's source
// throughout. Whether that knowledge is worth anything to the runner's source
// afterwards is exactly the question — pair history is entangled with the
// vantage that observed it, while a belief about a directed channel's
// liquidity is a fact about the channel.
func (r *SimRunner) RunScenarioFrom(source route.Vertex,
	s *SimScenario) (*SimScenarioResult, error) {

	if r.graph.Node(source) == nil {
		return nil, fmt.Errorf("source node %v not in graph", source)
	}

	result := &SimScenarioResult{Scenario: *s}

	// Let virtual time pass and background traffic move liquidity before
	// this payment starts, the way a live network keeps churning between
	// a node's own sends.
	r.advanceGap()

	target, err := r.graph.ResolveNode(s.Target)
	if err != nil {
		return nil, err
	}

	maxParts := s.MaxParts
	if maxParts == 0 {
		maxParts = 16
	}

	spec := &SimPaymentSpec{
		Target:   target,
		Amount:   lnwire.MilliSatoshi(s.AmtMsat),
		MaxParts: maxParts,
	}

	// Build the routing strategy under test for this payment, handing it
	// the public graph view and the sender's exact local balances. The
	// view wrapper hides the concrete graph so that a candidate router
	// cannot reach the hidden balances.
	router, err := r.routerFactory(
		&simGossipView{g: r.graph, now: r.clk.Now}, source,
		r.graph.LocalBalances(source), spec,
	)
	if err != nil {
		return nil, err
	}

	var (
		nextAttemptID uint64
		amtRemaining  = spec.Amount
		inFlightHtlcs uint32

		// holdIDs are the shards that have reached the destination but
		// are not settled yet, only ever populated under atomic mpp.
		// Their amount and fees ride along until the whole set either
		// settles or is released.
		holdIDs  []uint64
		heldMsat uint64
		heldFees uint64
	)

	// Under atomic mpp a payment that never completes settles nothing:
	// every shard still held when the loop exits gives its reserved
	// liquidity back, so a failed mpp leaves the hidden balances exactly
	// as it found them and charges no fees. The success path settles the
	// set and clears holdIDs before returning, so this only ever fires on
	// a failure path, whichever one it is.
	defer func() {
		if len(holdIDs) == 0 {
			return
		}

		for _, id := range holdIDs {
			r.graph.ReleaseHold(id)
		}
		result.HeldReleasedMsat = heldMsat
	}()

	for len(result.Attempts) < simMaxAttempts {
		// Ask the router for the next route to attempt.
		rt, err := router.RequestRoute(amtRemaining, inFlightHtlcs)
		if err != nil {
			result.Error = err.Error()
			break
		}

		// Send the htlc through the simulated network. A malformed
		// route (unknown channel, disconnected hops) is a router bug:
		// it terminates this payment with an error rather than
		// killing the whole batch, so one bad edge case doesn't zero
		// out an otherwise functional candidate.
		attemptID := nextAttemptID
		nextAttemptID++

		// Each attempt consumes virtual time: htlcs take real seconds
		// to resolve on a live network.
		r.advanceAttempt(s.AtomicMpp)

		// An atomic shard is held at the destination rather than
		// settled there, reserving the liquidity of every hop it
		// crossed until the payment as a whole resolves.
		var (
			htlcResult SimHtlcResult
			holdID     uint64
		)
		if s.AtomicMpp {
			htlcResult, holdID, err = r.graph.HoldHtlc(rt)
		} else {
			htlcResult, err = r.graph.SendHtlc(rt)
		}
		if err != nil {
			result.Error = fmt.Sprintf("malformed route: %v", err)
			break
		}

		result.Attempts = append(
			result.Attempts, traceAttempt(rt, htlcResult),
		)

		// Let the router learn from the outcome. The feedback is the
		// same either way: what atomic mpp changes is the price of a
		// serial probe, not the information it returns.
		err = router.ReportAttempt(attemptID, rt, htlcResult)
		if err != nil {
			return nil, err
		}

		if htlcResult.Failure != nil {
			continue
		}

		inFlightHtlcs++

		// A settling shard pays its fee right away; a held one only
		// pays when the whole set settles.
		if s.AtomicMpp {
			holdIDs = append(holdIDs, holdID)
			heldMsat += uint64(rt.ReceiverAmt())
			heldFees += uint64(rt.TotalFees())
		} else {
			result.FeeMsat += uint64(rt.TotalFees())
		}

		// Guard against a buggy router delivering more than asked:
		// unsigned underflow here would loop until the attempt cap.
		recv := rt.ReceiverAmt()
		if recv > amtRemaining {
			result.Error = "router over-delivered payment amount"
			break
		}
		amtRemaining -= recv

		if amtRemaining == 0 {
			// The full amount has arrived, so the held set becomes
			// real balance movement all at once and the fees it
			// carried finally come due. Without atomic mpp there
			// is nothing held and this is a no-op.
			for _, id := range holdIDs {
				r.graph.SettleHold(id)
			}
			holdIDs = nil
			result.FeeMsat += heldFees

			result.Success = true
			break
		}
	}

	return result, nil
}

// traceAttempt converts a route and its resolution into a trace record.
func traceAttempt(rt *route.Route, res SimHtlcResult) SimAttemptTrace {
	trace := SimAttemptTrace{
		AmtMsat: uint64(rt.TotalAmount),
		FeeMsat: uint64(rt.TotalFees()),
		Success: res.Failure == nil,
	}

	for _, hop := range rt.Hops {
		trace.Hops = append(trace.Hops, SimHopTrace{
			ChanID:  hop.ChannelID,
			PubKey:  hop.PubKeyBytes.String(),
			AmtMsat: uint64(hop.AmtToForward),
		})
	}

	if res.Failure != nil {
		trace.Failure = res.Failure.Code().String()
		if idx := getNodeIndexSim(rt, res.FailureSource); idx != nil {
			trace.FailureIdx = *idx
		}
	}

	return trace
}

// getNodeIndexSim returns the zero-based index of the given node in the
// route, nil if the node is not part of it.
func getNodeIndexSim(rt *route.Route, failureSource route.Vertex) *int {
	if failureSource == rt.SourcePubKey {
		idx := 0
		return &idx
	}

	for i, h := range rt.Hops {
		if h.PubKeyBytes == failureSource {
			idx := i + 1
			return &idx
		}
	}

	return nil
}

// simBandwidthHints exposes the sender's exact local balances to path
// finding, mirroring a real node's first-hop knowledge.
type simBandwidthHints struct {
	balances map[uint64]lnwire.MilliSatoshi
}

func (h *simBandwidthHints) availableChanBandwidth(channelID uint64,
	_ lnwire.MilliSatoshi) (lnwire.MilliSatoshi, bool) {

	balance, ok := h.balances[channelID]
	return balance, ok
}

func (h *simBandwidthHints) isCustomHTLCPayment() bool {
	return false
}

// SnapshotLiquidity captures the hidden balances of the network so that
// they can be put back later. See SimGraph.SnapshotLiquidity.
func (r *SimRunner) SnapshotLiquidity() *LiquiditySnapshot {
	return r.graph.SnapshotLiquidity()
}

// RestoreLiquidity puts the hidden balances back to a snapshot, and does
// nothing when given a nil snapshot.
func (r *SimRunner) RestoreLiquidity(snap *LiquiditySnapshot) {
	r.graph.RestoreLiquidity(snap)
}
