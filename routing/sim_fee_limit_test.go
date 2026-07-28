package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestSimFeeBudgetMsat checks the ppm arithmetic against hand-computed values,
// including the two cases the split multiplication exists for: an amount whose
// remainder carries the whole budget, and an amount large enough that the
// direct product would leave the range of a uint64.
func TestSimFeeBudgetMsat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		amt  lnwire.MilliSatoshi
		ppm  uint32
		want lnwire.MilliSatoshi
	}{
		{
			name: "no limit",
			amt:  1_000_000_000,
			ppm:  0,
			want: lnwire.MaxMilliSatoshi,
		},
		{
			name: "3000 ppm of a million sats",
			amt:  1_000_000_000,
			ppm:  3_000,
			want: 3_000_000,
		},
		{
			name: "one ppm is the smallest real budget",
			amt:  1_000_000,
			ppm:  1,
			want: 1,
		},
		{
			// The quotient is zero here, so the whole budget comes
			// out of the remainder term.
			name: "amount below a million msat",
			amt:  999_999,
			ppm:  500_000,
			want: 499_999,
		},
		{
			// Rounding is down, as it is everywhere else a fee is
			// computed: a budget is what the sender WILL pay.
			name: "rounds down",
			amt:  1_500_000,
			ppm:  1,
			want: 1,
		},
		{
			// 21e14 msat times 1e7 ppm overflows a uint64 if it is
			// multiplied out directly. Split, it does not.
			name: "whole supply at a thousand percent",
			amt:  2_100_000_000_000_000,
			ppm:  10_000_000,
			want: 21_000_000_000_000_000,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.want,
				simFeeBudgetMsat(test.amt, test.ppm),
			)
		})
	}
}

// TestSimRemainingBudget checks that the budget left after committed fees is
// lnd's calcFeeBudget: the difference, floored at zero on overrun.
func TestSimRemainingBudget(t *testing.T) {
	t.Parallel()

	require.EqualValues(t, 700, simRemainingBudget(1_000, 300))
	require.EqualValues(t, 0, simRemainingBudget(1_000, 1_000))
	require.EqualValues(t, 0, simRemainingBudget(1_000, 1_500))
	require.Equal(
		t, lnwire.MaxMilliSatoshi,
		simRemainingBudget(lnwire.MaxMilliSatoshi, 0),
	)
}

// feeLimitSpecRunner builds a runner over the two-path fixture whose router
// records the payment spec it was handed and then abandons the payment, which
// is all a test of the contract surface needs: the spec is delivered at
// construction, before any route is requested.
func feeLimitSpecRunner(t *testing.T) (*SimRunner, *[]lnwire.MilliSatoshi) {
	t.Helper()

	graph, nodes := atomicTestGraph(t)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), nodes[0], t.TempDir(),
	)
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	var seen []lnwire.MilliSatoshi
	runner.SetRouterFactory(func(_ SimNetworkView, _ route.Vertex,
		_ map[uint64]lnwire.MilliSatoshi,
		spec *SimPaymentSpec) (SimRouter, error) {

		seen = append(seen, spec.FeeLimitMsat)

		return &scriptedRouter{}, nil
	})

	return runner, &seen
}

// TestSimFeeLimitAbsentGolden is the identity claim of stage C's contract
// half: a scenario that names no fee limit hands its router the same unlimited
// budget the lnd arm has been constructed with since the program began. If
// this number ever changes, every published result moves, because a finite
// budget is a constraint on which routes exist.
func TestSimFeeLimitAbsentGolden(t *testing.T) {
	t.Parallel()

	runner, seen := feeLimitSpecRunner(t)

	_, err := runner.RunScenario(&SimScenario{
		Target:   "4",
		AmtMsat:  100_000_000,
		MaxParts: 1,
	})
	require.NoError(t, err)

	require.Equal(t, []lnwire.MilliSatoshi{lnwire.MaxMilliSatoshi}, *seen)
}

// TestSimFeeLimitReachesTheSpec is the other half of the golden above: a limit
// that IS named arrives at the router as a real budget, so the sentinel is a
// sentinel rather than an accident of dead plumbing.
func TestSimFeeLimitReachesTheSpec(t *testing.T) {
	t.Parallel()

	runner, seen := feeLimitSpecRunner(t)

	_, err := runner.RunScenario(&SimScenario{
		Target:      "4",
		AmtMsat:     100_000_000,
		MaxParts:    1,
		FeeLimitPPM: 3_000,
	})
	require.NoError(t, err)

	require.Equal(t, []lnwire.MilliSatoshi{300_000}, *seen)
}
