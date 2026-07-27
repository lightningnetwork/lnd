package routing

import (
	"fmt"
	"math/rand"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// The simulator's failure channel is perfect: every attempt comes back
// instantly, truthfully, and attributed to the exact node that failed. A real
// sender has none of those guarantees. A BOLT4 onion error can be garbled or
// forged, in which case the sender learns only THAT the payment failed and
// not where; a buggy or adversarial hop can return an error that looks
// perfectly well formed while pointing at somebody else; and every error
// arrives after a delay during which the network keeps moving.
//
// Every attempt-efficiency number this program has published was measured on
// the perfect channel, so all of them are upper bounds. The knobs below
// degrade the channel so that the bound can be measured against a version of
// the world that resembles mainnet's.

// SimAttributionParams degrades the failure channel between the simulator and
// the router under test. The zero value is the perfect channel every result
// before exp-019 was measured on, so a scenario file that omits the section
// runs exactly as it always has.
type SimAttributionParams struct {
	// UnknownProb is the probability that a FAILED attempt reaches the
	// router with its attribution stripped: the failure source is
	// replaced by a node that is not on the route and the failure code by
	// an unreadable marker, which is what a sender holds after an onion
	// error it cannot decrypt.
	UnknownProb float64 `json:"unknown_prob"`

	// ShiftProb is the probability that a failed attempt is blamed on a
	// node ADJACENT to the one that really failed, one hop before or
	// after it on a fair coin. The failure code is untouched, so the
	// router receives a well formed, entirely plausible, wrong answer.
	// This is the nastier of the two: an unknown failure announces that
	// it carries no information, while a shifted one does not.
	//
	// The draw only happens when the unknown draw did not fire, so the
	// two probabilities describe disjoint outcomes.
	ShiftProb float64 `json:"shift_prob"`

	// DelaySlices is how many attempt-sized slices of virtual time pass
	// between the network resolving an attempt and the router being told
	// about it. Each slice runs the background traffic engine for that
	// stretch, exactly as the atomic arena does between attempts, so the
	// evidence describes a network that has since moved. On a static tier
	// (no virtual clock, no traffic) this is a no-op by construction.
	DelaySlices int `json:"delay_slices"`

	// Seed seeds the degradation rng. Zero derives one deterministically
	// from the scenario's liquidity seed, so omitting it is still
	// reproducible.
	Seed int64 `json:"seed"`
}

// SimAttributionStats counts what the degradation actually did, so that a
// sweep can check the realized rates against the configured ones instead of
// assuming they took effect.
type SimAttributionStats struct {
	// Attempts is how many attempt results passed through the degradation
	// point, scored and warmup alike.
	Attempts int `json:"attempts"`

	// Unknown is how many of them reached the router unattributed.
	Unknown int `json:"unknown"`

	// Shifted is how many were blamed on an adjacent node. An attempt
	// whose shift draw fired but which had nowhere to shift to (no
	// attribution to move) is not counted here, so this is what the
	// router actually received rather than what was drawn.
	Shifted int `json:"shifted"`

	// Delayed is how many attempt results were held back for at least one
	// slice of virtual time. A run with delay_slices set on a static tier
	// reports zero, which is the honest answer: there was no time for the
	// result to go stale in.
	Delayed int `json:"delayed"`
}

// SimUnknownFailure is what a failure looks like to a sender that could not
// read the onion error: the htlc failed, and that is the whole of the
// message. It deliberately reports no meaningful failure code, so a router
// that switches on the code lands in its default branch rather than being
// told something untrue.
//
// The failure message stays non-nil because a nil one means SETTLED
// everywhere in the simulator; "failed, no information" and "succeeded" are
// very different facts and this type keeps them apart.
type SimUnknownFailure struct{}

// Code returns the empty failure code.
//
// NOTE: Part of the lnwire.FailureMessage interface.
func (SimUnknownFailure) Code() lnwire.FailCode {
	return lnwire.CodeNone
}

// Error returns a human readable description of the unreadable failure.
//
// NOTE: Part of the lnwire.FailureMessage interface.
func (SimUnknownFailure) Error() string {
	return "unreadable failure message"
}

// simUnknownSource is the failure source of an unattributed failure. The zero
// vertex is not a valid compressed pubkey, so it can never be a node of the
// graph and never appears on a route: every "where did this fail" lookup in
// the simulator, in lnd's stack and in the evolved routers alike, returns
// "not on the route" for it, which is precisely the fact being modelled.
var simUnknownSource = route.Vertex{}

// simAttributionDraws is how many uniform draws each attempt consumes,
// whatever its outcome. Holding it fixed makes the degradation sequence a
// function of the attempt index alone, so two routers on the same scenario
// face the same sequence rather than one of them shifting the stream by
// failing more often.
const simAttributionDraws = 3

// simAttribution degrades attempt results on their way from the simulator to
// the router under test.
type simAttribution struct {
	params SimAttributionParams
	rng    *rand.Rand
	stats  SimAttributionStats
}

// newSimAttribution validates a degradation config and builds the degrader.
// defaultSeed is the scenario's liquidity seed, used to derive an rng seed
// when the config does not pin one.
func newSimAttribution(params *SimAttributionParams,
	defaultSeed int64) (*simAttribution, error) {

	if params.UnknownProb < 0 || params.UnknownProb > 1 {
		return nil, fmt.Errorf("unknown_prob %v out of range [0,1]",
			params.UnknownProb)
	}
	if params.ShiftProb < 0 || params.ShiftProb > 1 {
		return nil, fmt.Errorf("shift_prob %v out of range [0,1]",
			params.ShiftProb)
	}
	if params.DelaySlices < 0 {
		return nil, fmt.Errorf("delay_slices %v is negative",
			params.DelaySlices)
	}

	seed := params.Seed
	if seed == 0 {
		seed = simAttributionSeed(defaultSeed)
	}

	return &simAttribution{
		params: *params,
		rng:    rand.New(rand.NewSource(seed)),
	}, nil
}

// simAttributionSeed derives the degradation seed from the scenario's
// liquidity seed. It is a single step of the usual linear congruential mixer,
// which keeps the two streams from lining up while staying a pure function of
// a value the scenario file already pins.
func simAttributionSeed(liquiditySeed int64) int64 {
	return liquiditySeed*6364136223846793005 + 1442695040888963407
}

// degrade returns what the router should be told about an attempt, given what
// really happened. The caller keeps the truthful result for its own
// bookkeeping: only the copy handed to the router is degraded, and only ever
// in the direction of less information, never more.
func (a *simAttribution) degrade(rt *route.Route,
	result SimHtlcResult) SimHtlcResult {

	a.stats.Attempts++

	// Every attempt consumes the same draws whether or not it can use
	// them. See simAttributionDraws.
	var draws [simAttributionDraws]float64
	for i := range draws {
		draws[i] = a.rng.Float64()
	}

	// A settled attempt has no attribution to lose. It is still counted
	// and still consumed its draws, so the sequence stays aligned.
	if result.Failure == nil || rt == nil {
		return result
	}

	// An unreadable onion error: the sender knows the payment failed and
	// nothing else.
	if draws[0] < a.params.UnknownProb {
		a.stats.Unknown++

		return SimHtlcResult{
			FailureSource: simUnknownSource,
			Failure:       SimUnknownFailure{},
		}
	}

	if draws[1] >= a.params.ShiftProb {
		return result
	}

	// Blame the neighbour. A failure that is already unattributed has
	// nothing to move, and neither has a route of no hops.
	idx := getNodeIndexSim(rt, result.FailureSource)
	if idx == nil {
		return result
	}

	shifted, ok := simAdjacentIndex(rt, *idx, draws[2] < 0.5)
	if !ok {
		return result
	}

	source, ok := simRouteNodeAt(rt, shifted)
	if !ok {
		return result
	}

	a.stats.Shifted++
	result.FailureSource = source

	return result
}

// countDelay records that one attempt result was held back.
func (a *simAttribution) countDelay() {
	a.stats.Delayed++
}

// simAdjacentIndex returns the index of a node one hop away from idx on the
// route, preferring the earlier neighbour when back is set. A neighbour that
// would fall off the end of the route is clamped by trying the other
// direction instead, and a route with only one node has no neighbour at all.
func simAdjacentIndex(rt *route.Route, idx int, back bool) (int, bool) {
	step := 1
	if back {
		step = -1
	}

	last := len(rt.Hops)
	for _, candidate := range []int{idx + step, idx - step} {
		if candidate >= 0 && candidate <= last && candidate != idx {
			return candidate, true
		}
	}

	return 0, false
}

// simRouteNodeAt returns the node at the given route index, where index zero
// is the sender and index i+1 is the i-th hop. It is the inverse of
// getNodeIndexSim.
func simRouteNodeAt(rt *route.Route, idx int) (route.Vertex, bool) {
	if idx == 0 {
		return rt.SourcePubKey, true
	}
	if idx < 0 || idx > len(rt.Hops) {
		return route.Vertex{}, false
	}

	return rt.Hops[idx-1].PubKeyBytes, true
}
