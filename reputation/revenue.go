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
func (a *aggregatedWindowAverage) add(value int64,
	ts time.Time) (int64, error) {

	return a.inner.add(value, ts)
}

// windowsTracked returns the (fractional) number of windows (periods) elapsed
// since start. It errors if the time precedes start, since a negative number of
// elapsed periods is not meaningful.
func (a *aggregatedWindowAverage) windowsTracked(ts time.Time) (float64,
	error) {

	if ts.Before(a.start) {
		return 0, errBackwardsTime
	}

	return ts.Sub(a.start).Seconds() / a.windowDuration.Seconds(), nil
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
func (a *aggregatedWindowAverage) valueAt(ts time.Time) (int64, error) {
	periods, err := a.windowsTracked(ts)
	if err != nil {
		return 0, err
	}

	warmup := a.warmupFactor(periods)

	raw, err := a.inner.valueAt(ts)
	if err != nil {
		return 0, err
	}

	return satFromFloat(math.Round(float64(raw) / warmup)).Int64(), nil
}
