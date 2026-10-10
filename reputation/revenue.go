package reputation

import (
	"math"
	"time"
)

// aggregatedWindowAverage tracks an average value over multiple rolling
// windows. Aggregating over several windows rather than reading a single one
// smooths out volatility, which makes the average harder to move quickly by
// manipulating recent activity.
//
// It wraps a single decaying average over windowDuration*windowCount and, when
// reading, divides by a warm-up factor so that a brief history does not read as
// an artificially low average (see warmupFactor).
type aggregatedWindowAverage struct {
	start          time.Time
	windowCount    uint8
	windowDuration time.Duration
	inner          *decayingAverage
}

// newAggregatedWindowAverage creates an aggregated average starting at zero as
// of start, tracking value over windowCount windows each of windowDuration.
func newAggregatedWindowAverage(window time.Duration, windowCount uint8,
	start time.Time) *aggregatedWindowAverage {

	return &aggregatedWindowAverage{
		start:          start,
		windowCount:    windowCount,
		windowDuration: window,
		inner: newDecayingAverage(
			start, window*time.Duration(windowCount),
		),
	}
}

// add records a value at the given time.
func (a *aggregatedWindowAverage) add(value int64, ts time.Time) int64 {
	return a.inner.add(value, ts)
}

// windowsTracked returns the (fractional) number of windows (periods) elapsed
// since start. A time before start means the clock went backwards; it counts as
// zero periods and is logged as an error.
func (a *aggregatedWindowAverage) windowsTracked(ts time.Time) float64 {
	if ts.Before(a.start) {
		log.Errorf("Reputation revenue read at %v, before its start "+
			"at %v: clock went backwards, counting no periods", ts,
			a.start)

		return 0
	}

	return ts.Sub(a.start).Seconds() / a.windowDuration.Seconds()
}

// warmupFactor returns the warm-up divisor for the number of periods
// (fractional windows) elapsed so far:
//
//	warmup = windowCount * (1 - exp(-periods / windowCount))
//
// As periods grows this converges to windowCount (the steady-state divisor).
// It is guarded at 1 to avoid the periods->0 singularity where the factor tends
// to 0 and would over-inflate the average.
func (a *aggregatedWindowAverage) warmupFactor(periods float64) float64 {
	count := float64(a.windowCount)

	warmup := count * (1 - math.Exp(-periods/count))
	if warmup < 1 {
		warmup = 1
	}

	return warmup
}

// valueAt returns the windowed average value as of the given time.
func (a *aggregatedWindowAverage) valueAt(ts time.Time) int64 {
	warmup := a.warmupFactor(a.windowsTracked(ts))
	raw := a.inner.valueAt(ts)

	return satFromFloat(math.Round(float64(raw) / warmup)).Int64()
}
