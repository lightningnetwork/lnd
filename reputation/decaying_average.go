package reputation

import (
	"errors"
	"math"
	"time"
)

// errBackwardsTime is returned when a decaying average is asked to evaluate at
// a timestamp earlier than its last update. The algorithm assumes monotonic
// time; this mirrors the LDK reference's InvalidTimestamp error.
var errBackwardsTime = errors.New("timestamp precedes last update")

// decayingAverage tracks a value that decays exponentially over a rolling
// window. It is the building block for both outgoing-channel reputation and
// (via aggregatedWindowAverage) the incoming-revenue threshold.
//
// The arithmetic deliberately mirrors the LDK reference (rust-lightning
// PR #4468) so that the two implementations agree numerically:
//   - decayRate = 0.5^(2/windowSecs), giving a true half-life at the window
//     midpoint.
//   - the running value is an int64 with saturating addition.
//   - decay applies value*decayRate^elapsed, rounded to the nearest integer.
type decayingAverage struct {
	value       int64
	lastUpdated uint64 // unix seconds
	windowSecs  float64
	decayRate   float64
}

// newDecayingAverage creates a decaying average that starts at zero as of the
// provided start timestamp, decaying over the given window.
func newDecayingAverage(start uint64, window time.Duration) *decayingAverage {
	windowSecs := window.Seconds()

	return &decayingAverage{
		value:       0,
		lastUpdated: start,
		windowSecs:  windowSecs,
		decayRate:   decayRateForWindow(windowSecs),
	}
}

// decayRateForWindow computes the per-second decay rate for a window expressed
// in seconds: 0.5^(2/windowSecs).
func decayRateForWindow(windowSecs float64) float64 {
	return math.Pow(0.5, 2.0/windowSecs)
}

// valueAt decays the stored value forward to the given timestamp, updates the
// internal state, and returns the decayed value. It errors if the timestamp is
// before the last update.
func (d *decayingAverage) valueAt(ts uint64) (int64, error) {
	if ts < d.lastUpdated {
		return 0, errBackwardsTime
	}

	elapsed := float64(ts - d.lastUpdated)
	d.value = clampFloatToInt64(
		math.Round(float64(d.value) * math.Pow(d.decayRate, elapsed)),
	)
	d.lastUpdated = ts

	return d.value, nil
}

// add decays the value to the given timestamp and then adds the provided
// (possibly negative) value using saturating arithmetic.
func (d *decayingAverage) add(value int64, ts uint64) (int64, error) {
	if _, err := d.valueAt(ts); err != nil {
		return 0, err
	}

	d.value = saturatingAddInt64(d.value, value)
	d.lastUpdated = ts

	return d.value, nil
}

// saturatingAddInt64 adds two int64 values, clamping to the int64 range on
// overflow/underflow rather than wrapping (matches Rust's saturating_add).
func saturatingAddInt64(a, b int64) int64 {
	sum := a + b
	switch {
	// Overflow: both operands positive but sum went negative.
	case a > 0 && b > 0 && sum < 0:
		return math.MaxInt64

	// Underflow: both operands negative but sum went non-negative.
	case a < 0 && b < 0 && sum >= 0:
		return math.MinInt64

	default:
		return sum
	}
}

// saturatingI64FromU64 converts a uint64 to int64, clamping to MaxInt64 on
// overflow (matches Rust's i64::try_from(...).unwrap_or(i64::MAX)).
func saturatingI64FromU64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}

	return int64(v)
}

// clampFloatToInt64 converts a float64 to int64, saturating to the int64 range
// on overflow. This is essential: Go's float→int conversion is undefined when
// the value is out of range and in practice yields MinInt64, so a decayed value
// that rounds above MaxInt64 (reachable precisely because add() saturates)
// would silently flip negative. Rust's `as i64` saturates; we match that.
func clampFloatToInt64(f float64) int64 {
	switch {
	// float64(math.MaxInt64) rounds up to 2^63, so >= is the correct
	// saturation boundary on the high end.
	case f >= float64(math.MaxInt64):
		return math.MaxInt64

	case f <= float64(math.MinInt64):
		return math.MinInt64

	default:
		return int64(f)
	}
}
