package reputation

import (
	"math"
	"time"
)

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

// restoreDecayingAverage rebuilds a decaying average from persisted state. The
// value and its timestamp are taken verbatim: decay is applied lazily on read,
// so the first read decays the value over the whole time the state was not
// live, including any downtime. Re-stamping the timestamp to the load time
// would silently skip that decay.
func restoreDecayingAverage(value int64, lastUpdated time.Time,
	window time.Duration) *decayingAverage {

	return &decayingAverage{
		value:       satFromInt(value),
		lastUpdated: lastUpdated,
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

// valueAt returns the stored value decayed forward to the given time. It is
// read-only: the internal state is only mutated by add, so that frequent reads
// do not accumulate rounding error.
func (d *decayingAverage) valueAt(ts time.Time) int64 {
	decayed := satFromFloat(
		math.Round(float64(d.value.Int64()) *
			math.Pow(d.decayRate, d.elapsed(ts))),
	)

	return decayed.Int64()
}

// elapsed returns the seconds between the last update and the given time. The
// algorithm assumes monotonic time, so a time before the last update means the
// clock went backwards: it is clamped to zero elapsed time, keeping the value
// undecayed, and logged as an error.
func (d *decayingAverage) elapsed(ts time.Time) float64 {
	if ts.Before(d.lastUpdated) {
		log.Errorf("Reputation average read at %v, before its last "+
			"update at %v: clock went backwards, applying no decay",
			ts, d.lastUpdated)

		return 0
	}

	return ts.Sub(d.lastUpdated).Seconds()
}

// add decays the value to the given time and then adds the provided (possibly
// negative) value. This is the only operation that mutates the stored value.
// A time before the last update adds the value without decay and leaves the
// last update time where it was.
func (d *decayingAverage) add(value int64, ts time.Time) int64 {
	d.value = satFromInt(d.valueAt(ts)).Add(satFromInt(value))
	if ts.After(d.lastUpdated) {
		d.lastUpdated = ts
	}

	return d.value.Int64()
}
