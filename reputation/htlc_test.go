package reputation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestOpportunityCostVectors checks opportunityCost against values computed
// directly from its formula
// max(0, (resolution_time - resolution_period)/resolution_period) * fees, with
// resolution_period = 90s and fee = 100. E.g. 135s -> (135-90)/90*100 = 50.
func TestOpportunityCostVectors(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig() // ResolutionPeriod = 90s.

	tests := []struct {
		resolution time.Duration
		want       uint64
	}{
		{10 * time.Second, 0},
		{90 * time.Second, 0},
		{91 * time.Second, 1},
		{135 * time.Second, 50},
		{180 * time.Second, 100},
		{900 * time.Second, 900},
	}

	for _, tc := range tests {
		got := cfg.opportunityCost(tc.resolution, 100)
		require.Equalf(t, tc.want, got, "opportunityCost(%v)",
			tc.resolution)
	}
}

// TestEffectiveFeeMatrix covers all four branches of the effective-fee matrix.
func TestEffectiveFeeMatrix(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	const fee = 1000

	// Vectors covering the effective_fee matrix. fast (45s) is within the
	// resolution period, so opportunity_cost = 0. slow (270s) gives
	// opportunity_cost = (270-90)/90*fee = 2*fee = 2000, so the
	// failed-accountable branch is -2000.
	fast := cfg.ResolutionPeriod / 2 // 45s, within period.
	slow := cfg.ResolutionPeriod * 3 // 270s.

	tests := []struct {
		name        string
		resolution  time.Duration
		accountable bool
		settled     bool
		want        int64
	}{
		{"accountable settled fast", fast, true, true, fee},
		{"accountable settled slow", slow, true, true, -fee},
		{"accountable failed fast", fast, true, false, 0},
		{"accountable failed slow", slow, true, false, -2 * fee},
		{"unaccountable settled fast", fast, false, true, fee},
		{"unaccountable settled slow", slow, false, true, 0},
		{"unaccountable failed fast", fast, false, false, 0},
		{"unaccountable failed slow", slow, false, false, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := cfg.effectiveFee(
				fee, tc.resolution, tc.accountable, tc.settled,
			)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestInFlightRisk checks the worst-case-hold opportunity cost.
func TestInFlightRisk(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	// cltv delta of 1 block = 600s hold. overrun = (600-90)/90 = 5.666...,
	// * fee(100) = 566.67 -> round 567.
	require.EqualValues(t, 567, cfg.inFlightRisk(100, 101, 100))

	// No delta -> zero hold -> zero risk.
	require.EqualValues(t, 0, cfg.inFlightRisk(100, 100, 100),
		"zero cltv delta must carry no risk")
}
