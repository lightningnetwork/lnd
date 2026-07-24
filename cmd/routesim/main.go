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

	// Scenarios are executed in order against a shared mission control.
	Scenarios []routing.SimScenario `json:"scenarios"`
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

	switch *router {
	case "lnd":
	case "candidate":
		runner.SetRouterFactory(newCandidateRouter)
	default:
		fatalf("unknown router %q", *router)
	}

	// Run all scenarios sequentially against the shared mission control.
	out := output{}
	for i := range scenFile.Scenarios {
		scenario := scenFile.Scenarios[i]

		result, err := runner.RunScenario(&scenario)
		if err != nil {
			fatalf("scenario %d failed: %v", i, err)
		}

		out.Aggregate.NumScenarios++
		out.Aggregate.TotalAttempts += len(result.Attempts)
		if result.Success {
			out.Aggregate.NumSuccesses++
			out.Aggregate.TotalFeeMsat += result.FeeMsat
			out.Aggregate.AmtSuccessMsat += scenario.AmtMsat
		}

		if !*traces {
			result.Attempts = nil
		}
		out.Results = append(out.Results, result)
	}

	agg := &out.Aggregate
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
