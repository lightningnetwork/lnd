package fee

import (
	"math"
	"math/bits"

	"github.com/lightningnetwork/lnd/fn/v2"
)

const maxInboundRate int64 = 10 * int64(rateParts)

// ClampInboundRate returns an inbound fee rate limited to the range used by
// inbound fee calculations.
func ClampInboundRate(rate int32) int64 {
	r := int64(rate)
	if r > maxInboundRate {
		return maxInboundRate
	}
	if r < -maxInboundRate {
		return -maxInboundRate
	}

	return r
}

// Inbound returns the signed inbound fee, capping the rate at ten million
// parts and rounding the proportional component toward zero. An error is
// returned if the final result cannot be represented as an int64.
func Inbound(amount uint64, base, rate int32) fn.Result[int64] {
	r := ClampInboundRate(rate)

	negative := r < 0
	if negative {
		r = -r
	}
	hi, lo := bits.Mul64(amount, uint64(r))
	if hi >= rateParts {
		return fn.Err[int64](errOverflow)
	}
	q, _ := bits.Div64(hi, lo, rateParts)
	if !negative {
		return Combined(q, int64(base))
	}

	if base >= 0 {
		b := uint64(base)
		if b >= q {
			return fn.Ok(int64(b - q))
		}
		q -= b
	} else {
		var carry uint64
		q, carry = bits.Add64(q, uint64(-int64(base)), 0)
		if carry != 0 {
			return fn.Err[int64](errOverflow)
		}
	}
	if q > uint64(1)<<63 {
		return fn.Err[int64](errOverflow)
	}
	if q == 0 {
		return fn.Ok(int64(0))
	}

	return fn.Ok(-int64(q-1) - 1)
}

// Combined adds an unsigned outbound fee and signed inbound fee without
// discarding cancellation. An error is returned if the signed sum overflows.
func Combined(out uint64, inbound int64) fn.Result[int64] {
	if inbound >= 0 {
		sum, carry := bits.Add64(out, uint64(inbound), 0)
		if carry != 0 || sum > math.MaxInt64 {
			return fn.Err[int64](errOverflow)
		}

		return fn.Ok(int64(sum))
	}

	magnitude := uint64(-(inbound + 1)) + 1
	if out >= magnitude {
		diff := out - magnitude
		if diff > math.MaxInt64 {
			return fn.Err[int64](errOverflow)
		}

		return fn.Ok(int64(diff))
	}
	diff := magnitude - out

	return fn.Ok(-int64(diff-1) - 1)
}

// AddAmount adds a signed fee to an unsigned amount, rejecting underflow and
// overflow.
func AddAmount(amount uint64, delta int64) fn.Result[uint64] {
	if delta >= 0 {
		sum, carry := bits.Add64(amount, uint64(delta), 0)
		if carry != 0 {
			return fn.Err[uint64](errOverflow)
		}

		return fn.Ok(sum)
	}

	magnitude := uint64(-(delta + 1)) + 1
	if amount < magnitude {
		return fn.Err[uint64](errOverflow)
	}

	return fn.Ok(amount - magnitude)
}
