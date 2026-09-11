package reputation

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testStart is the base time used by the tests that drive the averages
// directly.
var testStart = time.Unix(1_000_000, 0)

// TestDecayingAverageDecay verifies the decay e^(-elapsed/window) and the
// add-then-decay sequencing: the value falls to 1/sqrt(e) of itself after half
// a window and to 1/e after a full window, and an add applies on top of the
// value decayed to the add's timestamp.
func TestDecayingAverageDecay(t *testing.T) {
	t.Parallel()

	const window = 100 * time.Second
	d := newDecayingAverage(testStart, window)

	_, err := d.add(1000, testStart)
	require.NoError(t, err)

	// At half a window (50s): 1000 * e^(-0.5) = 606.5 -> 607.
	got, err := d.valueAt(testStart.Add(50 * time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 607, got, "half window")

	// At a full window (another 50s): 1000 * e^(-1) = 367.9 -> 368.
	got, err = d.valueAt(testStart.Add(100 * time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 368, got, "full window")

	// valueAt is read-only, so adding at 50s still works after the reads
	// above: the value decays to 607 and the add lands at 1607.
	v, err := d.add(1000, testStart.Add(50*time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 1607, v)
}

// TestDecayingAverageBackwardsTime ensures a backwards timestamp errors, since
// the decay assumes monotonic time.
func TestDecayingAverageBackwardsTime(t *testing.T) {
	t.Parallel()

	d := newDecayingAverage(testStart, time.Hour)
	_, err := d.valueAt(testStart.Add(-50 * time.Second))
	require.ErrorIs(t, err, errBackwardsTime)
}

// TestDecayingAverageOverflowClamp verifies that evaluating a saturated
// (near-MaxInt64) value does not flip negative. Because float64(MaxInt64)
// rounds up to 2^63, a naive int64(math.Round(...)) cast yields MinInt64; the
// clamp must keep it saturated at MaxInt64.
func TestDecayingAverageOverflowClamp(t *testing.T) {
	t.Parallel()

	const window = 100 * time.Second
	d := newDecayingAverage(testStart, window)

	// Saturate the running value to MaxInt64.
	_, err := d.add(math.MaxInt64, testStart)
	require.NoError(t, err)
	require.EqualValues(
		t, math.MaxInt64, d.value.Int64(), "setup: value not saturated",
	)

	// Evaluating at the same timestamp (no decay) round-trips the value
	// through float64; without the clamp this overflows to MinInt64.
	got, err := d.valueAt(testStart)
	require.NoError(t, err)
	require.EqualValues(t, int64(math.MaxInt64), got, "a negative value "+
		"means the float->int64 cast overflowed")
}

// TestAggregatedWindowWarmup verifies the warm-up factor
// windowCount*(1 - exp(-periods/windowCount)), guarded at 1.
func TestAggregatedWindowWarmup(t *testing.T) {
	t.Parallel()

	// window = 100s, windowCount = 6 -> inner window 600s.
	a := newAggregatedWindowAverage(100*time.Second, 6, testStart)

	// Add 600 at t=0. periods=0 => warmup factor tends to 0 and is guarded
	// to 1, so the value reads back as 600 (no decay at t=0).
	_, err := a.add(600, testStart)
	require.NoError(t, err)

	got, err := a.valueAt(testStart)
	require.NoError(t, err)
	require.EqualValues(t, 600, got, "warmup t=0")

	// At t=300 (periods=3), the inner value has decayed by e^(-300/600) to
	// 364, and the warm-up factor is 6*(1 - exp(-3/6)) = 2.3608..., so
	// 364/2.3608 rounds to 154.
	got, err = a.valueAt(testStart.Add(300 * time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 154, got, "warmup t=300")
}

// TestAggregatedWindowBackwardsTime ensures reading an aggregated average
// before its start timestamp errors rather than underflowing the unsigned
// elapsed-time subtraction.
func TestAggregatedWindowBackwardsTime(t *testing.T) {
	t.Parallel()

	a := newAggregatedWindowAverage(100*time.Second, 6, testStart)
	before := testStart.Add(-50 * time.Second)

	_, err := a.valueAt(before)
	require.ErrorIs(t, err, errBackwardsTime)

	_, err = a.windowsTracked(before)
	require.ErrorIs(t, err, errBackwardsTime)
}
