package routing

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/routing/route"
)

// SimLatencyParams prices an htlc attempt in virtual time by the route it
// travelled, replacing the flat per-attempt tick the clock section has charged
// since virtual time existed here at all.
//
// The flat tick is UNIFORM: every attempt costs attempt_sec whether it crossed
// one hop or nine, and whether it settled at the far end or was refused by the
// sender's own peer. Probing near is therefore exactly as expensive as probing
// far, which is neither how a payment network behaves nor how a sender learns.
// Under this section an attempt costs
//
//	attempt_overhead + 2 * per_hop * k
//
// where k is how many hops the htlc actually traversed before it resolved: the
// whole route on a settle, and the failing hop on a failure. That asymmetry is
// the mechanism rather than a detail of it. A failure at the sender's own first
// hop comes back in one round trip and a failure at hop eight comes back in
// eight, so a router that probes near before probing far learns faster in wall
// time even when it learns exactly as much per attempt.
//
// It is DIFFERENTIAL, and that is the whole reason to expect anything from it
// after exp-019 measured uniform delay as free for everyone. The attribution
// section's delay knob holds every result back by the same number of
// attempt-sized slices regardless of the route, so it can shift absolute time
// and nothing else; no evolved router's behavior depends on absolute time,
// since all of them dropped time decay. A knob that changes RELATIVE time can
// change which route is worth trying first. The two compose rather than
// conflict, and a file may set both: latency decides when a result is due, and
// the delay knob then ages the network further at the moment it arrives.
//
// Absent, an attempt costs the flat tick, which is what every scenario file
// written before stage E says by omission.
type SimLatencyParams struct {
	// PerHopMs is how long an htlc takes to cross one hop, ONE WAY, in
	// milliseconds. An attempt that reaches hop k pays it 2k times, out
	// and back, which is the round trip the sender actually waits through.
	PerHopMs float64 `json:"per_hop_ms"`

	// AttemptOverheadMs is what an attempt costs before any hop is
	// crossed: the sender's own path finding, onion construction and
	// commitment signing, in milliseconds. It is the part of the flat tick
	// that really is flat, and it is charged even on a route of one hop.
	AttemptOverheadMs float64 `json:"attempt_overhead_ms"`

	// HoldCarry asks that the time an htlc spends in the air be charged to
	// everything else that reads the clock: the background traffic through
	// the existing prorating, and the liquidity an atomic shard reserves
	// while it waits.
	//
	// The scheduler already does exactly this, unconditionally, by the same
	// rule it applies to every other stretch of virtual time (stage D:
	// traffic runs when nothing is live or when anything live is atomic).
	// So the field is accepted for schema fidelity and only true is
	// honored; false is REFUSED by name rather than silently ignored,
	// because opting the latency window out of the exogenous process would
	// mean a latency section quietly editing exp-014's traffic engine.
	//
	// A nil pointer is true, so a file may simply omit it.
	HoldCarry *bool `json:"hold_carry,omitempty"`
}

// validate rejects a latency section the runner cannot honor, naming the reason
// where one exists rather than running something else under the same name.
func (p *SimLatencyParams) validate() error {
	if p == nil {
		return nil
	}

	if p.PerHopMs < 0 {
		return fmt.Errorf("latency: per_hop_ms must not be negative, "+
			"got %v", p.PerHopMs)
	}

	if p.AttemptOverheadMs < 0 {
		return fmt.Errorf("latency: attempt_overhead_ms must not be "+
			"negative, got %v", p.AttemptOverheadMs)
	}

	// A section that charges nothing is not the flat tick, it is free
	// attempts, and a file that means the flat tick says so by omission.
	if p.PerHopMs == 0 && p.AttemptOverheadMs == 0 {
		return fmt.Errorf("latency: per_hop_ms and attempt_overhead_ms " +
			"are both zero, which makes an attempt take no time at " +
			"all; omit the section for the clock's flat attempt_sec")
	}

	if p.HoldCarry != nil && !*p.HoldCarry {
		return fmt.Errorf("latency: hold_carry false is REFUSED; the " +
			"scheduler charges the time an htlc spends in the air " +
			"to the background traffic and to whatever an atomic " +
			"shard reserves, by the same rule it applies to every " +
			"other stretch of virtual time, and a latency section " +
			"is not the place to edit the traffic engine")
	}

	return nil
}

// simLatency is the runtime form of the section, with the two step sizes
// converted once rather than per attempt.
type simLatency struct {
	params SimLatencyParams

	// overhead is what an attempt costs before it crosses a hop, and is
	// what replaces the clock's flat attempt tick between the moment a
	// route is chosen and the moment its htlc reaches the wire.
	overhead time.Duration

	// perHop is the one-way crossing time of a single hop.
	perHop time.Duration
}

// newSimLatency validates the section and converts it.
func newSimLatency(params *SimLatencyParams) (*simLatency, error) {
	if err := params.validate(); err != nil {
		return nil, err
	}

	return &simLatency{
		params: *params,
		overhead: time.Duration(
			params.AttemptOverheadMs * float64(time.Millisecond),
		),
		perHop: time.Duration(
			params.PerHopMs * float64(time.Millisecond),
		),
	}, nil
}

// returnTrip is how long the sender waits between an htlc reaching the wire and
// its result reaching the sender: the round trip to the hop that resolved it.
func (l *simLatency) returnTrip(hops int) time.Duration {
	if hops <= 0 {
		return 0
	}

	return 2 * time.Duration(hops) * l.perHop
}

// simLatencyHops is how many hops an htlc traversed before it resolved, which
// is what the round trip is charged on.
//
// A settle traversed the whole route. A failure traversed as far as the hop
// that produced it, which is one PAST the index of the node that reported it:
// walkHtlc blames the sending end of the hop it refused, so the node at index i
// reporting a failure means hop i+1 is the one that did not carry the htlc.
// That is the spec's own reading, and it is why a failure on the sender's own
// first hop costs one round trip rather than nothing. It is capped at the route
// length for the failures the FINAL node reports, where the htlc did reach the
// far end and there is no further hop to charge for.
//
// It is computed from the TRUE result rather than the one the router is told.
// An attribution section can move the blame to a neighbour of the node that
// really failed, and the clock is not something a damaged failure message gets
// to edit: the time an htlc took is a fact about the network, not about what
// came back over it.
func simLatencyHops(rt *route.Route, res SimHtlcResult) int {
	if res.Failure == nil {
		return len(rt.Hops)
	}

	idx := getNodeIndexSim(rt, res.FailureSource)
	if idx == nil {
		// A failure from a node that is not on the route at all is not
		// something walkHtlc produces. Charging the whole route is the
		// conservative reading: the htlc is only known to have stopped
		// somewhere, so it is not credited with having stopped early.
		return len(rt.Hops)
	}

	if hops := *idx + 1; hops < len(rt.Hops) {
		return hops
	}

	return len(rt.Hops)
}

// SimLatencyStats is what a batch reports about the time its payments took.
// None of it enters the objective: makespan_sec stays out of it for this whole
// program, and objective L is an offline re-scoring of archived runs rather
// than a change to what the optimizer maximizes.
//
// The pair is the MANIPULATION CHECK, and stage E is the fifth in a row to need
// one. A latency section that never fires and a latency section that fires and
// costs nothing are the same score, and only the realized times separate them.
// Read MeanAttemptLatencySec against the flat tick the clock section would have
// charged: equal means the mechanism is inert, and the per-attempt latency in
// the trace says whether it is differential.
type SimLatencyStats struct {
	// MeanPaymentLatencySec is the mean virtual time a scored payment took,
	// from the moment it was admitted to the moment it resolved. It counts
	// the waiting a payment does behind its own attempts, which is what a
	// sender actually experiences and what objective L is charged on.
	MeanPaymentLatencySec float64

	// MeanAttemptLatencySec is the mean virtual time one htlc attempt took,
	// dispatch to result. It is the differential number: on a tier where
	// failures come back early it sits below the settle round trip, and on
	// a tier with no latency section it is exactly the clock's attempt_sec.
	MeanAttemptLatencySec float64
}
