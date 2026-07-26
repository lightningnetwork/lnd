package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

// batchTestRunner builds a runner over a small synthetic network for the
// batch tests, with the stock lnd routing stack.
func batchTestRunner(t *testing.T) *routing.SimRunner {
	t.Helper()

	graph, err := routing.GenerateSimGraph(&routing.SimTopologySpec{
		Type:           "smallworld",
		NumNodes:       40,
		ChannelSizeSat: 1_000_000,
		Seed:           11,
		AvgDegree:      6,
	})
	require.NoError(t, err)

	require.NoError(t, graph.AssignLiquidity(routing.LiquidityBimodal, 3))

	source, err := graph.ResolveNode("1")
	require.NoError(t, err)
	require.NoError(t, graph.BalanceNodeChannels(source))

	runner, err := routing.NewSimRunner(
		graph, routing.DefaultSimParams(), source, t.TempDir(),
	)
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	return runner
}

// batchScenarios builds a payment per target.
func batchScenarios(targets ...string) []routing.SimScenario {
	scenarios := make([]routing.SimScenario, 0, len(targets))
	for _, target := range targets {
		scenarios = append(scenarios, routing.SimScenario{
			Target:   target,
			AmtMsat:  5_000_000,
			MaxParts: 2,
		})
	}

	return scenarios
}

// TestRunBatchWarmupUnscored asserts that the warmup payments stay out of the
// scored batch entirely: they never reach the results array, they never move
// a scored metric, and the only trace they leave in the output is the pair of
// counters that says what they cost.
func TestRunBatchWarmupUnscored(t *testing.T) {
	t.Parallel()

	scored := batchScenarios("10", "20", "30")
	scenFile := &scenarioFile{
		Warmup: &warmupSection{
			Scenarios: batchScenarios("15", "25"),
		},
		Scenarios: scored,
	}

	out, err := runBatch(batchTestRunner(t), scenFile, true)
	require.NoError(t, err)

	agg := out.Aggregate
	require.Equal(t, len(scored), agg.NumScenarios)
	require.Len(t, out.Results, len(scored))
	require.Equal(t, len(scenFile.Warmup.Scenarios), agg.WarmupScenarios)
	require.Positive(t, agg.WarmupAttempts)

	// The scored results are the scored payments, in order, and nothing
	// else.
	for i, result := range out.Results {
		require.Equal(t, scored[i].Target, result.Scenario.Target)
	}

	// Per-scenario attempt traces survive by default, which is what makes
	// the attempts-by-payment-index warmup curve computable.
	require.NotEmpty(t, out.Results[0].Attempts)
}

// TestRunBatchNoWarmup asserts that a scenario file without a warmup section
// behaves exactly as it did before the phase existed: same scored batch, and
// an output that does not even mention the warmup.
func TestRunBatchNoWarmup(t *testing.T) {
	t.Parallel()

	scored := batchScenarios("10", "20", "30")

	warmed, err := runBatch(batchTestRunner(t), &scenarioFile{
		Warmup:    &warmupSection{Scenarios: batchScenarios("15", "25")},
		Scenarios: scored,
	}, true)
	require.NoError(t, err)

	cold, err := runBatch(batchTestRunner(t), &scenarioFile{
		Scenarios: scored,
	}, true)
	require.NoError(t, err)

	require.Equal(t, warmed.Aggregate.NumScenarios,
		cold.Aggregate.NumScenarios)
	require.Zero(t, cold.Aggregate.WarmupScenarios)
	require.Zero(t, cold.Aggregate.WarmupAttempts)

	// The warmup counters are omitted from the encoding when there is no
	// warmup phase, so the output of an untouched scenario file is
	// unchanged.
	encoded, err := json.MarshalIndent(cold, "", "  ")
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "warmup_")

	// A traceless run still drops the attempt traces, warmup or not.
	traceless, err := runBatch(batchTestRunner(t), &scenarioFile{
		Scenarios: scored,
	}, false)
	require.NoError(t, err)
	for _, result := range traceless.Results {
		require.Empty(t, result.Attempts)
	}
}

// TestRunBatchWarmupSource asserts that a warmup section can name its own
// sender, and that an unknown one is rejected rather than quietly warming
// from the file-level source.
func TestRunBatchWarmupSource(t *testing.T) {
	t.Parallel()

	scenFile := &scenarioFile{
		Warmup: &warmupSection{
			Source:    "7",
			Scenarios: batchScenarios("10", "20"),
		},
		Scenarios: batchScenarios("10", "20", "30"),
	}

	out, err := runBatch(batchTestRunner(t), scenFile, true)
	require.NoError(t, err)
	require.Equal(t, 2, out.Aggregate.WarmupScenarios)
	require.Positive(t, out.Aggregate.WarmupAttempts)
	require.Len(t, out.Results, 3)

	scenFile.Warmup.Source = "no-such-node"
	_, err = runBatch(batchTestRunner(t), scenFile, true)
	require.ErrorContains(t, err, "unable to resolve warmup source")
}

// TestRunBatchWarmupError asserts that a broken warmup payment is reported as
// such rather than silently scored or mistaken for a scored scenario.
func TestRunBatchWarmupError(t *testing.T) {
	t.Parallel()

	_, err := runBatch(batchTestRunner(t), &scenarioFile{
		Warmup: &warmupSection{
			Scenarios: batchScenarios("no-such-node"),
		},
		Scenarios: batchScenarios("10"),
	}, true)
	require.Error(t, err)
	require.True(
		t, strings.HasPrefix(err.Error(), "warmup scenario 0 failed:"),
		"unexpected error: %v", err,
	)
}
