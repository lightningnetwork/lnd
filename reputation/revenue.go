package reputation

import (
	"math"
	"time"
)

// aggregatedWindowAverage tracks an average value over multiple rolling
// windows to smooth out volatility, used for the incoming-revenue threshold.
//
// It wraps a single decaying average over windowDuration*windowCount and, when
// reading, divides by the number of windows actually tracked so far (capped at
// windowCount). During the warm-up period — before windowCount windows have
// elapsed — it divides by the elapsed window count instead, preventing an
// artificially low average from a brief history.
//
// This mirrors the LDK reference (rust-lightning PR #4468), which uses the
// elapsed-windows divisor rather than the smooth exp() warm-up factor described
// in the BOLT write-up.
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

// windowsTracked returns the (fractional) number of windows elapsed since
// start.
func (a *aggregatedWindowAverage) windowsTracked(ts uint64) float64 {
	elapsed := float64(ts - a.start)

	return elapsed / a.windowDuration.Seconds()
}

// valueAt returns the windowed average value as of the given timestamp.
func (a *aggregatedWindowAverage) valueAt(ts uint64) (int64, error) {
	if ts < a.start {
		return 0, errBackwardsTime
	}

	tracked := a.windowsTracked(ts)
	if tracked < 1.0 {
		tracked = 1.0
	}

	divisor := math.Min(tracked, float64(a.windowCount))

	raw, err := a.inner.valueAt(ts)
	if err != nil {
		return 0, err
	}

	return clampFloatToInt64(math.Round(float64(raw) / divisor)), nil
}
