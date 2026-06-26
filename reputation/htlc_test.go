package reputation

import (
	"testing"
	"time"
)

// TestOpportunityCostVectors ports the exact LDK reference vectors
// (resolution_period = 90s, fee = 100), confirming numeric parity.
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
		if got != tc.want {
			t.Fatalf("opportunityCost(%v): got %d, want %d",
				tc.resolution, got, tc.want)
		}
	}
}

// TestEffectiveFeeMatrix covers all four branches of the effective-fee matrix.
func TestEffectiveFeeMatrix(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	const fee = 1000

	// Exact LDK golden vectors (rust-lightning #4468 test_effective_fees):
	// fast_resolve = resolution_period / 2, slow_resolve = period * 3.
	// slow gives opportunity_cost = round((270-90)/90)*fee = 2*fee = 2000,
	// so the failed-accountable branch is -2000 (overrun = 2.0, not 1.0) —
	// exercising the >1.0 magnitude the previous vector missed.
	fast := cfg.ResolutionPeriod / 2 // 45s, within period.
	slow := cfg.ResolutionPeriod * 3 // 270s, overrun = 2.0.

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
			got := cfg.effectiveFee(
				fee, tc.resolution, tc.accountable, tc.settled,
			)
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestInFlightRisk checks the worst-case-hold opportunity cost.
func TestInFlightRisk(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	// cltv delta of 1 block = 600s hold. overrun = (600-90)/90 = 5.666...,
	// * fee(100) = 566.67 -> round 567.
	got := cfg.inFlightRisk(100, 101, 100)
	if got != 567 {
		t.Fatalf("inFlightRisk: got %d, want 567", got)
	}

	// No delta -> zero hold -> zero risk.
	if got := cfg.inFlightRisk(100, 100, 100); got != 0 {
		t.Fatalf("inFlightRisk zero-delta: got %d, want 0", got)
	}
}
