package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

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

	// Error records a terminal payment error, e.g. no path found.
	Error string `json:"error,omitempty"`
}

// SimRunner runs payment scenarios against a simulated network with a
// persistent mission control, mirroring the control loop of a real node.
type SimRunner struct {
	graph  *SimGraph
	source route.Vertex
	mc     *MissionControl
	params *SimParams

	// routerFactory builds the routing strategy under test, once per
	// payment. Defaults to the lnd production stack.
	routerFactory SimRouterFactory

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
		params:  params,
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

// RunScenario executes a single payment scenario and returns its trace. The
// mission control state carries over between scenarios, so ordering matters,
// just like consecutive payments on a real node.
func (r *SimRunner) RunScenario(s *SimScenario) (*SimScenarioResult, error) {
	result := &SimScenarioResult{Scenario: *s}

	source := r.source
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
		&simGossipView{g: r.graph}, source,
		r.graph.LocalBalances(source), spec,
	)
	if err != nil {
		return nil, err
	}

	var (
		nextAttemptID uint64
		amtRemaining  = spec.Amount
		inFlightHtlcs uint32
	)

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

		htlcResult, err := r.graph.SendHtlc(rt)
		if err != nil {
			result.Error = fmt.Sprintf("malformed route: %v", err)
			break
		}

		result.Attempts = append(
			result.Attempts, traceAttempt(rt, htlcResult),
		)

		// Let the router learn from the outcome.
		err = router.ReportAttempt(attemptID, rt, htlcResult)
		if err != nil {
			return nil, err
		}

		if htlcResult.Failure != nil {
			continue
		}

		inFlightHtlcs++
		result.FeeMsat += uint64(rt.TotalFees())

		// Guard against a buggy router delivering more than asked:
		// unsigned underflow here would loop until the attempt cap.
		recv := rt.ReceiverAmt()
		if recv > amtRemaining {
			result.Error = "router over-delivered payment amount"
			break
		}
		amtRemaining -= recv

		if amtRemaining == 0 {
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
