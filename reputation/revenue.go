package reputation

import (
	"math"
	"time"
)

// aggregatedWindowAverage tracks an average value over multiple rolling
// windows to smooth out volatility, used for the incoming-revenue threshold.
//
// It wraps a single decaying average over windowDuration*windowCount and, when
// reading, divides by a warm-up factor so that a brief history does not read as
// an artificially low average.
//
// Unlike the LDK reference (rust-lightning PR #4468), which divides by the
// (capped) count of elapsed windows, we follow the BOLT proposal's smooth
// exponential warm-up factor windowCount*(1 - exp(-periods/windowCount)). This
// is the one intentional divergence from LDK; see reputation/DESIGN.md. It
// avoids the periods->0 singularity and the early over-inflation of the
// integer-windows divisor.
type aggregatedWindowAverage struct {
	start          uint64 // unix seconds
	windowCount    uint8
	windowDuration time.Duration
	inner          *decayingAverage
}

// newAggregatedWindowAverage creates an aggregated average starting at zero as
// of start, tracking value over windowCount windows each of windowDuration.
func newAggregatedWindowAverage(window time.Duration, windowCount uint8,
	start uint64) *aggregatedWindowAverage {

	return &aggregatedWindowAverage{
		start:          start,
		windowCount:    windowCount,
		windowDuration: window,
		inner: newDecayingAverage(
			start, window*time.Duration(windowCount),
		),
	}
}

// add records a value at the given timestamp.
func (a *aggregatedWindowAverage) add(value int64, ts uint64) (int64, error) {
	return a.inner.add(value, ts)
}

// windowsTracked returns the (fractional) number of windows (periods) elapsed
// since start.
func (a *aggregatedWindowAverage) windowsTracked(ts uint64) float64 {
	elapsed := float64(ts - a.start)

	return elapsed / a.windowDuration.Seconds()
}

// warmupFactor returns the BOLT-proposal warm-up divisor for the number of
// periods (fractional windows) elapsed so far:
//
//	warmup = windowCount * (1 - exp(-periods / windowCount))
//
// As periods -> infinity this converges to windowCount (the steady-state
// divisor). It is guarded at 1 to avoid the periods->0 singularity (where the
// factor tends to 0) and the resulting early over-inflation of the average.
func (a *aggregatedWindowAverage) warmupFactor(periods float64) float64 {
	count := float64(a.windowCount)

	warmup := count * (1 - math.Exp(-periods/count))
	if warmup < 1 {
		warmup = 1
	}

	return warmup
}

// valueAt returns the windowed average value as of the given timestamp.
func (a *aggregatedWindowAverage) valueAt(ts uint64) (int64, error) {
	if ts < a.start {
		return 0, errBackwardsTime
	}

	warmup := a.warmupFactor(a.windowsTracked(ts))

	raw, err := a.inner.valueAt(ts)
	if err != nil {
		return 0, err
	}

	return clampFloatToInt64(math.Round(float64(raw) / warmup)), nil
}
