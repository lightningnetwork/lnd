// Command routesim runs payment scenarios against a simulated Lightning
// Network using lnd's production path finding and mission control stack. It
// is the evaluator backend for optimization runs: parameters go in, per
// attempt traces and aggregate metrics come out as JSON.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

// scenarioFile is the top-level input: one graph, one liquidity assignment,
// one source node and a sequence of payments made from it.
type scenarioFile struct {
	// Graph selects the network: either a describegraph JSON snapshot or
	// a synthetic topology.
	GraphFile string                   `json:"graph_file,omitempty"`
	Topology  *routing.SimTopologySpec `json:"topology,omitempty"`

	// Liquidity assigns hidden channel balances.
	LiquidityModel string `json:"liquidity_model"`
	LiquiditySeed  int64  `json:"liquidity_seed"`

	// UnbalancedSource skips rebalancing the source's own channels to a
	// 50/50 split after liquidity assignment. By default the source is
	// rebalanced so that failures reflect routing difficulty, not an
	// underfunded sender.
	UnbalancedSource bool `json:"unbalanced_source,omitempty"`

	// Source is the node all payments originate from.
	Source string `json:"source"`

	// Clock enables virtual time: simulated seconds pass between
	// payments and attempts, so decay half-lives actually operate.
	Clock *routing.SimClockParams `json:"clock,omitempty"`

	// BackgroundTraffic enables exogenous seeded payments that move
	// hidden liquidity between the scenario payments.
	BackgroundTraffic *routing.SimTrafficParams `json:"background_traffic,omitempty"`

	// Attribution degrades the failure channel: failures that arrive
	// unattributed, failures blamed on a neighbour of the node that
	// really failed, and results that arrive after the network has moved
	// on. Omitting the section keeps the perfect channel every earlier
	// experiment measured on.
	Attribution *routing.SimAttributionParams `json:"attribution,omitempty"`

	// HtlcLimits redraws the announced min and max htlc of every directed
	// policy from a named family, and switches the network onto uniform
	// first-hop enforcement while it is at it. Omitting the section keeps
	// the constants every synthetic tier has carried for the whole
	// program: a 1000 msat floor, no ceiling, and a sender exempt from its
	// own announced policy.
	HtlcLimits *routing.SimHtlcLimitsParams `json:"htlc_limits,omitempty"`

	// InboundFees switches on the inbound fee every real forwarding policy
	// may announce, and draws one per directed policy from a named family.
	// The as_loaded family draws nothing and prices what the network
	// already announces, which is how a describegraph snapshot's own
	// inbound fees get charged. Omitting the section leaves the mechanism
	// off entirely: nothing is charged at forwarding time and the gossip
	// view shows a zero fee, which is the world every published number was
	// measured in, snapshot tiers included.
	InboundFees *routing.SimInboundFeeParams `json:"inbound_fees,omitempty"`

	// FeeLimitPPM is the fee budget every payment in this file gets unless
	// it names one of its own, in parts per million of its own amount. It
	// is how a whole tier is put under fee pressure with one number, which
	// is what the stage C ladder needs; a scenario that pins its own limit
	// keeps it. Omitting it, like a scenario omitting it, means no limit.
	FeeLimitPPM uint32 `json:"fee_limit_ppm,omitempty"`

	// Warmup is an optional unscored phase that runs before the scored
	// batch, standing in for routing knowledge a node was handed instead
	// of having to probe for it. Omitting the section is a cold start,
	// which is what every batch has historically been.
	Warmup *warmupSection `json:"warmup,omitempty"`

	// Scenarios are executed in order against a shared mission control.
	Scenarios []routing.SimScenario `json:"scenarios"`
}

// warmupSection describes the unscored payments that precede the scored
// batch. They run through the very same code path as a scored payment, on the
// same runner, so everything a scored payment would leave behind they leave
// behind too: mission control history, whatever cross-payment state the
// router under test keeps for itself, and real movement of hidden liquidity.
// Only their scores are dropped. What that measures is the value of a warm
// cache: the scored batch of a warmed run against the scored batch of a cold
// one.
type warmupSection struct {
	// Scenarios are the unscored payments, in order, identical in shape
	// to the scored ones.
	Scenarios []routing.SimScenario `json:"scenarios"`

	// Source optionally sends the warmup payments from a different node
	// than the scored batch, in the same node-reference format as the
	// file-level source. That is the third-party case: the knowledge in
	// the cache was gathered from somebody else's vantage, and only the
	// part of it that describes channels rather than the observer
	// transfers. Empty warms from the file-level source.
	Source string `json:"source,omitempty"`

	// StaleGapSec is how much virtual time passes between the warmup
	// payments and the scored batch, with the background traffic running
	// for that whole window. It ages the warmed knowledge the way served
	// weights age between the probe that gathered them and the payment
	// that uses them. Zero means the scored batch starts immediately.
	StaleGapSec float64 `json:"stale_gap_sec,omitempty"`

	// RestoreLiquidity puts the hidden balances back to what they
	// were before the warmup ran, so the scored batch measures what
	// the router LEARNED rather than what the warmup spent.
	RestoreLiquidity bool `json:"restore_liquidity,omitempty"`
}

// aggregate summarizes a batch of scenario results into the scalar signals
// an optimizer scores on.
type aggregate struct {
	NumScenarios int     `json:"num_scenarios"`
	NumSuccesses int     `json:"num_successes"`
	SuccessRate  float64 `json:"success_rate"`

	// TotalAttempts counts every htlc attempt, the dominant latency
	// driver in practice.
	TotalAttempts   int     `json:"total_attempts"`
	AttemptsPerScen float64 `json:"attempts_per_scenario"`
	TotalFeeMsat    uint64  `json:"total_fee_msat"`
	FeePPMOnSuccess float64 `json:"fee_ppm_on_success"`
	AmtSuccessMsat  uint64  `json:"amt_success_msat"`

	// TotalFeeMsatSpent is every millisatoshi of fee that actually left
	// the sender, INCLUDING on payments that then failed. TotalFeeMsat
	// above counts only the payments that completed, and on a non-atomic
	// tier, which is most of them, a partially settled mpp that later
	// fails has genuinely paid its forwarding nodes: that money was
	// missing from every number this program has published. Under
	// atomic_mpp a failed payment releases its shards and pays nothing, so
	// there the two are equal by construction.
	//
	// The leak was only ever in the aggregate. Each scenario result has
	// carried its own spent fee in fee_msat all along, so every run ever
	// archived can be re-totalled from its results array.
	TotalFeeMsatSpent uint64 `json:"total_fee_msat_spent"`

	// FeePPMAttempted is spent fees over the amount the batch was ASKED to
	// deliver, and it is the fee metric abandonment cannot launder.
	// FeePPMOnSuccess is a ratio over the payments that completed, so
	// dropping the most expensive payment in a file improves it
	// mechanically; here the abandoned amount stays in the denominator and
	// whatever the attempt spent stays in the numerator.
	//
	// It is REPORTED and not scored. The objective is unchanged in this
	// stage, and whether the fee term should migrate onto this metric is
	// the question the pre-registered side-by-side arm answers offline
	// from these very runs. Reading the two together is informative on its
	// own: they part company exactly when a router gives up.
	FeePPMAttempted float64 `json:"fee_ppm_attempted"`

	// NumGiveUps counts the scored payments the router ABANDONED, i.e.
	// terminated itself rather than running out of attempts. Nothing
	// scores it; it is here so that a candidate cannot buy a low attempt
	// count by quitting on payments it could have completed, which is
	// exactly what exp-013's winner did and what the composite objective
	// could not distinguish from efficiency.
	NumGiveUps int     `json:"num_give_ups"`
	GiveUpRate float64 `json:"give_up_rate"`

	// Import* report what a served-weights injection actually delivered.
	// ImportRouterAccepts is the one that prevents a silent null: a
	// candidate that does not implement the optional importer half of
	// the contract receives nothing, and "imports did not help" has to
	// be distinguishable from "imports were never delivered".
	ImportOffered       int  `json:"import_offered,omitempty"`
	ImportAccepted      int  `json:"import_accepted,omitempty"`
	ImportDroppedLocal  int  `json:"import_dropped_local,omitempty"`
	ImportRouterAccepts bool `json:"import_router_accepts,omitempty"`

	// BgPaymentsSent and BgPaymentsSettled report the background traffic
	// volume when the traffic model is enabled. BgSettleRate is the ratio
	// worth watching: a failed background payment moves no liquidity, so
	// the settle rate is the factor between the churn a scenario file
	// asks for and the churn it gets.
	BgPaymentsSent    int     `json:"bg_payments_sent,omitempty"`
	BgPaymentsSettled int     `json:"bg_payments_settled,omitempty"`
	BgSettleRate      float64 `json:"bg_settle_rate,omitempty"`

	// Attribution* report what the degraded failure channel actually did,
	// counting warmup attempts along with scored ones. They exist so that
	// a sweep can check the realized degradation against the configured
	// probabilities rather than assuming the section took effect;
	// AttributionDelayed in particular reads zero on a static tier, where
	// a delay is a no-op because there is no time for evidence to age in.
	AttributionAttempts int `json:"attribution_attempts,omitempty"`
	AttributionUnknown  int `json:"attribution_unknown,omitempty"`
	AttributionShifted  int `json:"attribution_shifted,omitempty"`
	AttributionDelayed  int `json:"attribution_delayed,omitempty"`

	// HtlcLimit* describe how binding the network's announced htlc limits
	// are: how many directed policies announce a ceiling below their
	// channel's capacity and how many announce a floor a real shard could
	// fall under. THIS is stage A's manipulation check. A tier whose
	// ceilings never bind is testing nothing, and these three counts are
	// what say whether it does.
	//
	// Htlc*Refusals count the htlcs those limits turned away at forwarding
	// time, and measured over 52 paired runs they are ZERO everywhere.
	// That is not a bug and it is worth stating plainly: every arm filters
	// on the announced limits before it sends. lnd's unified edges run
	// amtInRange, the seed candidate's usable() checks both fields, and
	// the background traffic engine filters on them too, so an announced
	// limit removes an edge at PLAN time and never gets the chance to
	// refuse an htlc on the wire. These counters are therefore an alarm
	// rather than a measurement: a non-zero reading means some router sent
	// an htlc its own gossip view told it would be refused, which is a
	// router bug worth knowing about and the only thing the uniform
	// first-hop rule can produce (HtlcSourceRefusals).
	//
	// The three static counts are reported only when some limit can bind
	// at all, so a tier carrying the generator's constants emits exactly
	// the output it always did.
	HtlcLimitPolicies  int `json:"htlc_limit_policies,omitempty"`
	HtlcLimitBounded   int `json:"htlc_limit_bounded,omitempty"`
	HtlcLimitFloors    int `json:"htlc_limit_floors,omitempty"`
	HtlcMinRefusals    int `json:"htlc_min_refusals,omitempty"`
	HtlcMaxRefusals    int `json:"htlc_max_refusals,omitempty"`
	HtlcSourceRefusals int `json:"htlc_source_refusals,omitempty"`

	// InboundFee* describe stage B, and they split the same way stage A's
	// counters do, for the same reason.
	//
	// The census (Policies, Charging, Discounts, Surcharges) is the
	// measurement. It is the ONLY thing here that can say a tier carries
	// inbound fees, because a discount changes what a sender is willing to
	// pay and nothing a forwarding node does, so it leaves no trace on the
	// wire whatsoever. Read the discount and surcharge counts separately:
	// only a surcharge can refuse an htlc, so a tier with none of them
	// cannot produce a refusal however heavily it prices.
	//
	// InboundFeeCharged counts the forwarding hops that priced a non-zero
	// inbound fee. It says the mechanism reached the wire, which is worth
	// having, and it says nothing about whether the mechanism mattered.
	//
	// InboundFeeRefusals is an ALARM, exactly like the htlc refusal
	// counters above and for the same structural reason: inbound fees are
	// priced at PLAN time. lnd's path finding adds the inbound fee to the
	// amount every candidate hop must send (pathfind.go's processEdge) and
	// the traffic engine does the same, so an arm that reads its own
	// gossip view reports zero here. A non-zero reading means some sender
	// underpaid a fee it was shown, which for an evolved candidate is the
	// expected starting point rather than a bug: no router in this program
	// has ever had a reason to look.
	//
	// The census is reported only when some policy announces a fee, so a
	// tier with the mechanism off emits exactly the output it always did.
	InboundFeePolicies   int `json:"inbound_fee_policies,omitempty"`
	InboundFeeCharging   int `json:"inbound_fee_charging,omitempty"`
	InboundFeeDiscounts  int `json:"inbound_fee_discounts,omitempty"`
	InboundFeeSurcharges int `json:"inbound_fee_surcharges,omitempty"`
	InboundFeeCharged    int `json:"inbound_fee_charged,omitempty"`
	InboundFeeRefusals   int `json:"inbound_fee_refusals,omitempty"`

	// FeeLimit* describe stage C, and they split the way stage A's and
	// stage B's counters do. This is the third time, and by now it is a
	// rule rather than a surprise: a constraint the arms can SEE binds at
	// plan time, so the counter that fires on the wire is an alarm and the
	// measurement has to come from somewhere else.
	//
	// FeeLimitPayments is the static half. It says a budget was
	// configured, exactly as the inbound fee census says a tier carries
	// inbound fees, and nothing more.
	//
	// FeeLimitFailures is the ALARM. lnd's path finding prunes any partial
	// path whose accumulated fee exceeds the budget it was handed, so an
	// arm that prices its own routes against its own budget never offers
	// the runner a route it has to refuse and reports ZERO here. A
	// non-zero reading names a router that proposed a route it had been
	// told it could not afford, which for an evolved candidate is the
	// expected starting point rather than a bug: none of them has ever had
	// a budget to respect.
	//
	// Neither says how much the budget MATTERED. A binding limit removes
	// routes at plan time, so its effect is entirely in which payments
	// complete and at what cost, and bindingness is read off success_rate
	// and fee_ppm_attempted against the unlimited control.
	//
	// Read FeeLimitFailures next to NumGiveUps, always. A router that
	// stops sending because everything is over budget and one that never
	// found a route are the same number in the objective, and these two
	// counters are the only things that separate them.
	FeeLimitPayments int `json:"fee_limit_payments,omitempty"`
	FeeLimitFailures int `json:"fee_limit_failures,omitempty"`

	// WarmupScenarios and WarmupAttempts report what the unscored warmup
	// phase cost. They are kept out of every metric above so that a warmed
	// run and a cold one are scored on exactly the same payments, while
	// the price of the warmup stays visible.
	WarmupScenarios int `json:"warmup_scenarios,omitempty"`
	WarmupAttempts  int `json:"warmup_attempts,omitempty"`
}

type output struct {
	Aggregate aggregate                    `json:"aggregate"`
	Results   []*routing.SimScenarioResult `json:"results"`
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	var (
		paramsPath = flag.String("params", "", "path to params "+
			"JSON (empty = lnd defaults)")
		scenariosPath = flag.String("scenarios", "", "path to "+
			"scenario file JSON (required)")
		outPath = flag.String("out", "", "path to write "+
			"results JSON (empty = stdout)")
		traces = flag.Bool("traces", true, "include per "+
			"attempt traces in the output")
		dumpDefaults = flag.Bool("dump-defaults", false, "print "+
			"the default params JSON (the optimization seed) "+
			"and exit")
		router = flag.String("router", "lnd", "routing "+
			"strategy: 'lnd' (production stack) or 'candidate' "+
			"(the algorithm in candidate_impl.go)")
		importWeights = flag.String("import-weights", "", "path to "+
			"a served observation file to inject BEFORE any "+
			"payment is sent. This is the only construction that "+
			"separates the value of knowledge from the cost of "+
			"acquiring it: a warmup phase buys its knowledge with "+
			"payments that drain the corridors they teach about, "+
			"while served weights arrive over an API for free")
		importLocal = flag.Bool("import-local", false, "also import "+
			"observations about the consumer's OWN channels. Off "+
			"by default: exp-012 measured lnd's attempt count "+
			"tripling when warmed from its own vantage, because "+
			"every payment it sends crosses its own first hop and "+
			"stale claims about those channels poison all of them")
		exportWeights = flag.String("export-weights", "", "path to "+
			"write everything this run observed, the server side "+
			"of the same API")
	)
	flag.Parse()

	if *dumpDefaults {
		encoded, err := json.MarshalIndent(
			routing.DefaultSimParams(), "", "  ",
		)
		if err != nil {
			fatalf("unable to encode defaults: %v", err)
		}
		fmt.Println(string(encoded))
		return
	}

	if *scenariosPath == "" {
		fatalf("--scenarios is required")
	}

	// Load the candidate parameters, defaulting to lnd's current
	// defaults, which are the natural optimization seed.
	params := routing.DefaultSimParams()
	if *paramsPath != "" {
		data, err := os.ReadFile(*paramsPath)
		if err != nil {
			fatalf("unable to read params: %v", err)
		}
		if err := json.Unmarshal(data, params); err != nil {
			fatalf("unable to parse params: %v", err)
		}
	}

	data, err := os.ReadFile(*scenariosPath)
	if err != nil {
		fatalf("unable to read scenarios: %v", err)
	}
	var scenFile scenarioFile
	if err := json.Unmarshal(data, &scenFile); err != nil {
		fatalf("unable to parse scenarios: %v", err)
	}

	// Build the network.
	var graph *routing.SimGraph
	switch {
	case scenFile.GraphFile != "":
		graph, err = routing.LoadSimGraphFromFile(scenFile.GraphFile)
	case scenFile.Topology != nil:
		graph, err = routing.GenerateSimGraph(scenFile.Topology)
	default:
		fatalf("scenario file must set graph_file or topology")
	}
	if err != nil {
		fatalf("unable to build graph: %v", err)
	}

	// Assign the hidden liquidity.
	model := routing.LiquidityModel(scenFile.LiquidityModel)
	if model == "" {
		model = routing.LiquidityUniform
	}
	if err := graph.AssignLiquidity(model, scenFile.LiquiditySeed); err != nil {
		fatalf("unable to assign liquidity: %v", err)
	}

	// Redraw the announced htlc limits, if this file asks for it. The
	// liquidity seed doubles as the limit seed when the section pins none,
	// so a corpus that varies only its liquidity seed still varies its
	// limits. Balances are untouched either way, so the order relative to
	// the assignment above is a matter of reading rather than of draws.
	err = graph.ApplyHtlcLimits(scenFile.HtlcLimits, scenFile.LiquiditySeed)
	if err != nil {
		fatalf("unable to apply htlc limits: %v", err)
	}

	// Same for the inbound fees, and the same seed sharing. On a loaded
	// snapshot the as_loaded family is the one that matters: it draws
	// nothing and only switches pricing on, so the snapshot's own 4,783
	// inbound fees are what gets charged.
	err = graph.ApplyInboundFees(scenFile.InboundFees, scenFile.LiquiditySeed)
	if err != nil {
		fatalf("unable to apply inbound fees: %v", err)
	}

	source, err := graph.ResolveNode(scenFile.Source)
	if err != nil {
		fatalf("unable to resolve source: %v", err)
	}

	if !scenFile.UnbalancedSource {
		if err := graph.BalanceNodeChannels(source); err != nil {
			fatalf("unable to balance source: %v", err)
		}
	}

	runner, err := routing.NewSimRunner(graph, params, source, "")
	if err != nil {
		fatalf("unable to create runner: %v", err)
	}
	defer runner.Close()

	if scenFile.Clock != nil {
		runner.SetVirtualClock(scenFile.Clock)
	}
	if scenFile.BackgroundTraffic != nil {
		err := runner.SetBackgroundTraffic(scenFile.BackgroundTraffic)
		if err != nil {
			fatalf("unable to enable traffic: %v", err)
		}

		// Point the focused share of the churn at this run's own
		// corridors: the source every payment leaves from, and every
		// target they head for, scored and warmup alike. Unresolvable
		// references are skipped rather than fatal, since a scenario
		// file may name a node the graph does not carry.
		focus := []route.Vertex{source}
		targets := make([]string, 0, len(scenFile.Scenarios))
		for i := range scenFile.Scenarios {
			targets = append(targets, scenFile.Scenarios[i].Target)
		}
		if scenFile.Warmup != nil {
			for i := range scenFile.Warmup.Scenarios {
				targets = append(
					targets,
					scenFile.Warmup.Scenarios[i].Target,
				)
			}
		}
		for _, ref := range targets {
			v, err := graph.ResolveNode(ref)
			if err != nil {
				continue
			}
			focus = append(focus, v)
		}
		runner.SetTrafficFocus(focus)
	}

	// Damage the failure channel, if this file asks for it. The liquidity
	// seed doubles as the degradation seed when the section pins none, so
	// a corpus that varies only its liquidity seed still varies its
	// degradation draws.
	if scenFile.Attribution != nil {
		err := runner.SetAttribution(
			scenFile.Attribution, scenFile.LiquiditySeed,
		)
		if err != nil {
			fatalf("unable to set attribution: %v", err)
		}
	}

	switch *router {
	case "lnd":
	case "candidate":
		runner.SetRouterFactory(newCandidateRouter)
	default:
		fatalf("unknown router %q", *router)
	}

	// Serve knowledge in before anything is spent acquiring it.
	var importStats *routing.SimImportStats
	if *importWeights != "" {
		importStats, err = runner.ImportWeightsFile(
			*importWeights, routing.SimImportPolicy{
				ExcludeLocal: !*importLocal,
			},
		)
		if err != nil {
			fatalf("unable to import weights: %v", err)
		}
	}

	out, err := runBatch(runner, &scenFile, *traces)
	if err != nil {
		fatalf("%v", err)
	}

	if importStats != nil {
		out.Aggregate.ImportOffered = importStats.Offered
		out.Aggregate.ImportAccepted = importStats.Accepted
		out.Aggregate.ImportDroppedLocal = importStats.DroppedLocal
		out.Aggregate.ImportRouterAccepts = importStats.RouterAccepts
	}

	if *exportWeights != "" {
		err := routing.WriteObservations(
			*exportWeights, runner.Observations(),
		)
		if err != nil {
			fatalf("unable to export weights: %v", err)
		}
	}

	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fatalf("unable to encode output: %v", err)
	}

	if *outPath == "" {
		fmt.Println(string(encoded))
		return
	}
	if err := os.WriteFile(*outPath, encoded, 0644); err != nil {
		fatalf("unable to write output: %v", err)
	}
}

// runBatch executes the optional unscored warmup phase and then the scored
// scenarios, all sequentially against the shared mission control, and returns
// the results with their aggregate metrics.
func runBatch(runner *routing.SimRunner, scenFile *scenarioFile,
	traces bool) (*output, error) {

	out := &output{}

	// The warmup payments are real payments: they probe, they learn, and
	// they move hidden liquidity. Only their results are dropped, so the
	// scored batch inherits both the knowledge they bought and the network
	// they perturbed on the way.
	if scenFile.Warmup != nil {
		// A warmup section may name its own sender, in which case the
		// scored batch still runs from the file-level source and only
		// inherits what crosses vantages.
		runWarmup := runner.RunScenario
		if scenFile.Warmup.Source != "" {
			source, err := runner.ResolveNode(
				scenFile.Warmup.Source,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to resolve "+
					"warmup source: %v", err)
			}

			runWarmup = func(s *routing.SimScenario) (
				*routing.SimScenarioResult, error) {

				return runner.RunScenarioFrom(source, s)
			}
		}

		// Warmup payments teach the router about the network and
		// drain that network at the same time. A served weight cache
		// hands a fresh node the knowledge without also having spent
		// the liquidity, so restoring the balances afterwards is what
		// isolates the value of the knowledge alone.
		var snapshot *routing.LiquiditySnapshot
		if scenFile.Warmup.RestoreLiquidity {
			snapshot = runner.SnapshotLiquidity()
		}

		for i := range scenFile.Warmup.Scenarios {
			scenario := scenFile.Warmup.Scenarios[i]
			applyFeeLimit(&scenario, scenFile.FeeLimitPPM)

			result, err := runWarmup(&scenario)
			if err != nil {
				return nil, fmt.Errorf("warmup scenario %d "+
					"failed: %v", i, err)
			}

			out.Aggregate.WarmupScenarios++
			out.Aggregate.WarmupAttempts += len(result.Attempts)
		}

		// Let the warmed knowledge age before it is put to use.
		runner.AdvanceIdle(scenFile.Warmup.StaleGapSec)

		runner.RestoreLiquidity(snapshot)
	}

	// amtAttemptedMsat is what the batch was asked to deliver, the
	// denominator of the fee ratio abandonment cannot launder. It is a
	// local rather than a reported field because it is exactly the sum of
	// the amounts already printed in the results array.
	var amtAttemptedMsat uint64

	for i := range scenFile.Scenarios {
		scenario := scenFile.Scenarios[i]
		applyFeeLimit(&scenario, scenFile.FeeLimitPPM)

		result, err := runner.RunScenario(&scenario)
		if err != nil {
			return nil, fmt.Errorf("scenario %d failed: %v", i, err)
		}

		out.Aggregate.NumScenarios++
		out.Aggregate.TotalAttempts += len(result.Attempts)
		amtAttemptedMsat += scenario.AmtMsat

		// Every millisatoshi of fee that left the sender counts here,
		// including on a payment that then failed, which is the whole
		// point of the field: the per-payment fee_msat has always
		// carried it and only the aggregate dropped it.
		out.Aggregate.TotalFeeMsatSpent += result.FeeMsat

		if result.Success {
			out.Aggregate.NumSuccesses++
			out.Aggregate.TotalFeeMsat += result.FeeMsat
			out.Aggregate.AmtSuccessMsat += scenario.AmtMsat
		} else if result.GaveUp {
			out.Aggregate.NumGiveUps++
		}

		if !traces {
			result.Attempts = nil
		}
		out.Results = append(out.Results, result)
	}

	agg := &out.Aggregate
	agg.BgPaymentsSent, agg.BgPaymentsSettled = runner.TrafficStats()

	limits := runner.HtlcLimitStats()
	if limits.Bounded > 0 || limits.Floors > 0 {
		agg.HtlcLimitPolicies = limits.Policies
		agg.HtlcLimitBounded = limits.Bounded
		agg.HtlcLimitFloors = limits.Floors
	}

	refusals := runner.PolicyStats()
	agg.HtlcMinRefusals = refusals.MinHtlcRefusals
	agg.HtlcMaxRefusals = refusals.MaxHtlcRefusals
	agg.HtlcSourceRefusals = refusals.SourceRefusals
	agg.InboundFeeCharged = refusals.InboundFeeCharged
	agg.InboundFeeRefusals = refusals.InboundFeeRefusals

	// The inbound fee census is emitted only when a scenario file asked
	// for the mechanism. The loader preserves a snapshot's real inbound
	// fees whether or not anything prices them, so a census printed
	// unconditionally would announce thousands of policies on a mainnet run
	// whose output has to stay identical to every mainnet run before stage
	// B. With the section absent the fees are dead data and a census of
	// dead data is worse than none.
	if scenFile.InboundFees != nil && scenFile.InboundFees.Family != "" {
		inbound := runner.InboundFeeStats()
		agg.InboundFeePolicies = inbound.Policies
		agg.InboundFeeCharging = inbound.Charging
		agg.InboundFeeDiscounts = inbound.Discounts
		agg.InboundFeeSurcharges = inbound.Surcharges
	}

	feeLimits := runner.FeeLimitStats()
	agg.FeeLimitPayments = feeLimits.Payments
	agg.FeeLimitFailures = feeLimits.Failures

	attribution := runner.AttributionStats()
	agg.AttributionAttempts = attribution.Attempts
	agg.AttributionUnknown = attribution.Unknown
	agg.AttributionShifted = attribution.Shifted
	agg.AttributionDelayed = attribution.Delayed

	if agg.NumScenarios > 0 {
		agg.SuccessRate = float64(agg.NumSuccesses) /
			float64(agg.NumScenarios)
		agg.AttemptsPerScen = float64(agg.TotalAttempts) /
			float64(agg.NumScenarios)
		agg.GiveUpRate = float64(agg.NumGiveUps) /
			float64(agg.NumScenarios)
	}
	if agg.BgPaymentsSent > 0 {
		agg.BgSettleRate = float64(agg.BgPaymentsSettled) /
			float64(agg.BgPaymentsSent)
	}
	if agg.AmtSuccessMsat > 0 {
		agg.FeePPMOnSuccess = 1e6 * float64(agg.TotalFeeMsat) /
			float64(agg.AmtSuccessMsat)
	}
	if amtAttemptedMsat > 0 {
		agg.FeePPMAttempted = 1e6 * float64(agg.TotalFeeMsatSpent) /
			float64(amtAttemptedMsat)
	}

	return out, nil
}

// applyFeeLimit gives a payment the file's default fee budget when it does not
// name one of its own. Zero means no limit at both levels, so a file that says
// nothing leaves every payment unbounded, which is what every scenario file
// written before stage C does.
func applyFeeLimit(scenario *routing.SimScenario, defaultPPM uint32) {
	if scenario.FeeLimitPPM == 0 {
		scenario.FeeLimitPPM = defaultPPM
	}
}
