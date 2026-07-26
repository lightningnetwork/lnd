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

	// BgPaymentsSent and BgPaymentsSettled report the background traffic
	// volume when the traffic model is enabled.
	BgPaymentsSent    int `json:"bg_payments_sent,omitempty"`
	BgPaymentsSettled int `json:"bg_payments_settled,omitempty"`

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
	}

	switch *router {
	case "lnd":
	case "candidate":
		runner.SetRouterFactory(newCandidateRouter)
	default:
		fatalf("unknown router %q", *router)
	}

	out, err := runBatch(runner, &scenFile, *traces)
	if err != nil {
		fatalf("%v", err)
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

		for i := range scenFile.Warmup.Scenarios {
			scenario := scenFile.Warmup.Scenarios[i]

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
	}

	for i := range scenFile.Scenarios {
		scenario := scenFile.Scenarios[i]

		result, err := runner.RunScenario(&scenario)
		if err != nil {
			return nil, fmt.Errorf("scenario %d failed: %v", i, err)
		}

		out.Aggregate.NumScenarios++
		out.Aggregate.TotalAttempts += len(result.Attempts)
		if result.Success {
			out.Aggregate.NumSuccesses++
			out.Aggregate.TotalFeeMsat += result.FeeMsat
			out.Aggregate.AmtSuccessMsat += scenario.AmtMsat
		}

		if !traces {
			result.Attempts = nil
		}
		out.Results = append(out.Results, result)
	}

	agg := &out.Aggregate
	agg.BgPaymentsSent, agg.BgPaymentsSettled = runner.TrafficStats()
	if agg.NumScenarios > 0 {
		agg.SuccessRate = float64(agg.NumSuccesses) /
			float64(agg.NumScenarios)
		agg.AttemptsPerScen = float64(agg.TotalAttempts) /
			float64(agg.NumScenarios)
	}
	if agg.AmtSuccessMsat > 0 {
		agg.FeePPMOnSuccess = 1e6 * float64(agg.TotalFeeMsat) /
			float64(agg.AmtSuccessMsat)
	}

	return out, nil
}
