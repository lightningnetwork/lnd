// Package fee provides checked forwarding fee arithmetic.
package fee

import (
	"errors"
	"math/bits"

	"github.com/lightningnetwork/lnd/fn/v2"
)

const rateParts uint64 = 1_000_000

var errOverflow = errors.New("forwarding fee arithmetic overflow")

// Outbound returns base plus the proportional fee, rounded down. An error is
// returned if the fee, or the amount plus the fee, cannot be represented as a
// uint64. The second bound is what every forwarder needs, since the incoming
// amount is the outgoing amount plus the fee.
func Outbound(amount, base, rate uint64) fn.Result[uint64] {
	hi, lo := bits.Mul64(amount, rate)
	if hi >= rateParts {
		return fn.Err[uint64](errOverflow)
	}

	proportional, _ := bits.Div64(hi, lo, rateParts)
	fee, carry := bits.Add64(base, proportional, 0)
	if carry != 0 {
		return fn.Err[uint64](errOverflow)
	}
	if _, carry := bits.Add64(amount, fee, 0); carry != 0 {
		return fn.Err[uint64](errOverflow)
	}

	return fn.Ok(fee)
}
