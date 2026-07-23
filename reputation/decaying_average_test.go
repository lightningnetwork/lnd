package reputation

import (
	"errors"
	"math"
	"testing"
	"time"
)

// TestDecayingAverageHalfLife verifies the value halves at the window midpoint,
// matching the LDK decay_rate = 0.5^(2/window) definition.
func TestDecayingAverageHalfLife(t *testing.T) {
	t.Parallel()

	const window = 100 * time.Second
	d := newDecayingAverage(0, window)

	if _, err := d.add(1000, 0); err != nil {
		t.Fatalf("add: %v", err)
	}

	// At the window midpoint (50s) the value should be halved.
	got, err := d.valueAt(50)
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}
	if got != 500 {
		t.Fatalf("half-life: got %d, want 500", got)
	}

	// At a full window (another 50s, total 100s) it should halve again.
	got, err = d.valueAt(100)
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}
	if got != 250 {
		t.Fatalf("full window: got %d, want 250", got)
	}
}

// TestDecayingAverageAddAndDecay checks add-then-decay sequencing.
func TestDecayingAverageAddAndDecay(t *testing.T) {
	t.Parallel()

	const window = 100 * time.Second
	d := newDecayingAverage(0, window)

	if _, err := d.add(1000, 0); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Decay to 50s -> 500, then add 1000 -> 1500.
	v, err := d.add(1000, 50)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if v != 1500 {
		t.Fatalf("got %d, want 1500", v)
	}
}

// TestDecayingAverageBackwardsTime ensures a backwards timestamp errors, like
// LDK's InvalidTimestamp guard.
func TestDecayingAverageBackwardsTime(t *testing.T) {
	t.Parallel()

	d := newDecayingAverage(100, time.Hour)
	if _, err := d.valueAt(50); !errors.Is(err, errBackwardsTime) {
		t.Fatalf("expected errBackwardsTime, got %v", err)
	}
}

// TestSaturatingAddInt64 checks clamping behavior.
func TestSaturatingAddInt64(t *testing.T) {
	t.Parallel()

	if got := saturatingAddInt64(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("overflow: got %d", got)
	}
	if got := saturatingAddInt64(math.MinInt64, -1); got != math.MinInt64 {
		t.Fatalf("underflow: got %d", got)
	}
	if got := saturatingAddInt64(5, -3); got != 2 {
		t.Fatalf("normal: got %d", got)
	}
}

// TestDecayingAverageOverflowClamp verifies that evaluating a saturated
// (near-MaxInt64) value does not flip negative (review finding B1). Because
// float64(MaxInt64) rounds up to 2^63, the naive int64(math.Round(...)) cast
// yields MinInt64; the clamp must keep it saturated at MaxInt64.
func TestDecayingAverageOverflowClamp(t *testing.T) {
	t.Parallel()

	const window = 100 * time.Second
	d := newDecayingAverage(0, window)

	// Saturate the running value to MaxInt64.
	if _, err := d.add(math.MaxInt64, 0); err != nil {
		t.Fatalf("add: %v", err)
	}
	if d.value != math.MaxInt64 {
		t.Fatalf("setup: value not saturated: %d", d.value)
	}

	// Evaluating at the same timestamp (no decay) round-trips the value
	// through float64; without the clamp this overflows to MinInt64.
	got, err := d.valueAt(0)
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}
	if got != math.MaxInt64 {
		t.Fatalf("overflow clamp: got %d, want MaxInt64 (negative "+
			"means the float->int64 cast overflowed)", got)
	}
}

// TestAggregatedWindowWarmup verifies the BOLT-proposal exponential warm-up
// factor windowCount*(1 - exp(-periods/windowCount)), guarded at 1 (our one
// intentional divergence from LDK's integer-windows divisor).
func TestAggregatedWindowWarmup(t *testing.T) {
	t.Parallel()

	// window = 100s, windowCount = 6 -> inner window 600s.
	a := newAggregatedWindowAverage(100*time.Second, 6, 0)

	// Add 600 at t=0. periods=0 => warmup factor tends to 0 and is guarded
	// to 1, so the value reads back as 600 (no decay at t=0).
	if _, err := a.add(600, 0); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := a.valueAt(0)
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}
	if got != 600 {
		t.Fatalf("warmup t=0: got %d, want 600", got)
	}

	// At t=300 (periods=3), the inner value has decayed by one half-life
	// (2*300/600 = 1) to 300, and the warm-up factor is
	// 6*(1 - exp(-3/6)) = 2.3608..., so 300/2.3608 rounds to 127.
	got, err = a.valueAt(300)
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}
	if got != 127 {
		t.Fatalf("warmup t=300: got %d, want 127", got)
	}
}
