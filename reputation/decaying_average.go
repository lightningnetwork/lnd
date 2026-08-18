package reputation

import (
	"errors"
	"math"
	"time"
)

// errBackwardsTime is returned when a decaying average is asked to evaluate at
// a timestamp earlier than its last update. The algorithm assumes monotonic
// time.
var errBackwardsTime = errors.New("timestamp precedes last update")

// decayingAverage tracks a value that decays exponentially over a rolling
// window. The running value saturates rather than wrapping (see saturatedI64).
type decayingAverage struct {
	value       saturatedI64
	lastUpdated time.Time
	decayRate   float64
}

// newDecayingAverage creates a decaying average that starts at zero as of the
// provided start time, decaying over the given window.
func newDecayingAverage(start time.Time,
	window time.Duration) *decayingAverage {

	return &decayingAverage{
		lastUpdated: start,
		decayRate:   decayRateForWindow(window),
	}
}

// decayRateForWindow computes the per-second decay rate for the given window.
// BOLT #1280 defines decay_rate = (1/2)^(1/(ln2 * window)); raised to elapsed
// seconds this is e^(-elapsed/window), so the value decays to 1/e of itself
// over a full window.
func decayRateForWindow(window time.Duration) float64 {
	return math.Pow(0.5, 1.0/(math.Ln2*window.Seconds()))
}

// valueAt decays the stored value forward to the given time, updates the
// internal state, and returns the decayed value. It errors if the time is
// before the last update.
func (d *decayingAverage) valueAt(ts time.Time) (int64, error) {
	if ts.Before(d.lastUpdated) {
		return 0, errBackwardsTime
	}

	elapsed := ts.Sub(d.lastUpdated).Seconds()
	d.value = satFromFloat(
		math.Round(float64(d.value.Int64()) *
			math.Pow(d.decayRate, elapsed)),
	)
	d.lastUpdated = ts

	return d.value.Int64(), nil
}

// add decays the value to the given time and then adds the provided (possibly
// negative) value.
func (d *decayingAverage) add(value int64, ts time.Time) (int64, error) {
	if _, err := d.valueAt(ts); err != nil {
		return 0, err
	}

	d.value = d.value.Add(satFromInt(value))
	d.lastUpdated = ts

	return d.value.Int64(), nil
}
