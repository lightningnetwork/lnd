package routing

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fees are charged, conserved and scored in this simulator, and nothing binds.
// The lnd arm has been built with FeeLimit: lnwire.MaxMilliSatoshi since the
// first payment this program ever ran, so path finding's own fee ceiling
// (pathfind.go's totalFee check) has never fired once. SimPaymentSpec carried
// no fee field at all, so no candidate has ever been told what it was allowed
// to spend. A fee-blind router was therefore never punished with a failure,
// only with a small subtraction in the objective.
//
// Stage C makes the budget part of the ENVIRONMENT rather than part of the
// weight. A payment carries a fee limit, the sender is told what it is, and a
// route that would spend more than the payment has left is not sent. That is a
// constraint every arm meets at the same place and on the same terms, which is
// the exp-019 construction: degradation and constraint live at the shared
// delivery point, never inside one router.
//
// The one thing this file deliberately does NOT do is change the objective.
// The give-up arithmetic in the design spec is the reason: the corpus files
// hold between 6 and 10 scored payments, so abandoning one payment in the
// smallest file costs 1/6 = 0.167 of objective while the entire fee term is
// worth at most FEE_PPM_CAP * FEE_WEIGHT = 0.100. That factor of 1.67 is the
// only thing standing between the fee term and the exp-013 give-up attractor,
// and it survives here because the weight is untouched. See simulation/
// evaluate.py, where the rule is recorded next to the constants that would
// break it.

// SimFeeLimitFailure is what a route refused for cost looks like to the router
// that proposed it: the htlc was never sent, because sending it would have
// spent more than the payment had left in its fee budget.
//
// It reports no meaningful failure code, for the same reason SimUnknownFailure
// does not: a router that switches on the code lands in its default branch
// rather than being told something untrue. There is no honest code to report,
// because this failure never crossed the wire. No forwarding node saw the
// htlc, no channel was probed, and NOTHING was learned about liquidity. A
// router that writes a liquidity bound from this result is writing fiction,
// which is why the runner keeps it out of the observation stream as well.
//
// The failure message stays non-nil because a nil one means SETTLED everywhere
// in the simulator, and it is attributed to the sender itself, which is the
// truth: we are the node that refused. lnd's own mission control already reads
// that shape correctly. processFail dispatches source index 0 to
// processPaymentOutcomeSelf, whose default branch penalizes nothing and logs
// that a local failure happened between planning and execution, which is
// exactly what a budget refusal is.
type SimFeeLimitFailure struct{}

// Code returns the empty failure code.
//
// NOTE: Part of the lnwire.FailureMessage interface.
func (SimFeeLimitFailure) Code() lnwire.FailCode {
	return lnwire.CodeNone
}

// Error returns a human readable description of the refusal.
//
// NOTE: Part of the lnwire.FailureMessage interface.
func (SimFeeLimitFailure) Error() string {
	return "route exceeds the payment's fee budget"
}

// simFeeLimitFailureName is what an attempt trace calls a budget refusal.
// Neither this failure nor an unreadable one carries a wire code, so naming it
// off the code alone would print the two identically and a reflection model
// reading the trace could not tell "I was refused for cost" from "I could not
// read the error".
const simFeeLimitFailureName = "FeeLimitExceeded"

// SimFeeLimitStats describes what a scenario batch's fee budgets did, and it
// splits the way stage A's and stage B's counters split, for the third time
// and for the same structural reason.
//
// Payments is the static half: how many payments carried a finite budget at
// all, counting warmup payments along with scored ones exactly as the stage A
// and stage B counters do. It says the mechanism was configured, the way the
// inbound fee census says a tier carries inbound fees.
//
// Failures is the ALARM half. A fee limit binds at PLAN time: lnd's path
// finding prunes any partial path whose accumulated fee exceeds the budget it
// was handed (pathfind.go's totalFee check), so an arm that prices its own
// routes against the budget it was given never offers one the runner has to
// refuse. An lnd arm wired correctly reports ZERO here. A non-zero reading
// names a router that proposed a route it had been told it could not afford,
// which for an evolved candidate is the expected starting point rather than a
// bug: no router in this program has ever had a budget to respect.
//
// Neither counter measures how much the budget MATTERED. A limit that binds
// removes routes at plan time and its whole effect is therefore in which
// payments complete and at what cost, so bindingness is read off the pair
// (success_rate, fee_ppm_attempted) against the unlimited control, the same
// way stage B's discounts had to be read off realized fees rather than off any
// wire counter.
type SimFeeLimitStats struct {
	// Payments is how many payments carried a finite fee budget.
	Payments int `json:"fee_limit_payments,omitempty"`

	// Failures is how many attempts the runner refused to dispatch because
	// the route would have overrun the payment's remaining budget.
	Failures int `json:"fee_limit_failures,omitempty"`
}

// simFeeBudgetMsat converts a payment's fee limit in parts per million of its
// own amount into the millisatoshi budget the runner enforces. A zero limit is
// how a scenario file says "no limit", which is the state every scenario file
// written before stage C is in, and it maps to the same lnwire.MaxMilliSatoshi
// the lnd arm has always been constructed with.
//
// The multiplication is split across the quotient and the remainder of the
// amount rather than done directly. amt*ppm can leave the range of a uint64 at
// amounts and limits the scenario schema permits, and the split form is exact:
// amt = q*1e6 + r gives floor(amt*ppm/1e6) = q*ppm + floor(r*ppm/1e6).
func simFeeBudgetMsat(amt lnwire.MilliSatoshi,
	ppm uint32) lnwire.MilliSatoshi {

	if ppm == 0 {
		return lnwire.MaxMilliSatoshi
	}

	quotient := uint64(amt) / 1_000_000
	remainder := uint64(amt) % 1_000_000

	return lnwire.MilliSatoshi(
		quotient*uint64(ppm) + remainder*uint64(ppm)/1_000_000,
	)
}

// simRemainingBudget returns what is left of a fee budget after the fees a
// payment has already committed, floored at zero on overrun.
//
// This is lnd's own calcFeeBudget (payment_lifecycle.go), deliberately: the
// runner's backstop and the fee limit handed to lnd's path finding have to be
// the same number computed the same way, or the backstop would fire on the lnd
// arm for a rounding difference rather than for a router ignoring its budget.
func simRemainingBudget(limit,
	committed lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	if committed >= limit {
		return 0
	}

	return limit - committed
}
