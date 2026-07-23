package reputation

import (
	"math"
	"sort"
)

// ChannelReputationSnapshot is a read-only view of one channel's computed
// reputation state, as of the snapshot time. All values are decayed forward to
// the snapshot time without mutating the live state.
type ChannelReputationSnapshot struct {
	// SCID is the short channel id.
	SCID uint64

	// OutgoingReputation is the reputation this channel has accrued as an
	// outgoing link, decayed to the snapshot time.
	OutgoingReputation int64

	// IncomingRevenue is the revenue threshold this channel has earned us
	// as an incoming link, decayed to the snapshot time.
	IncomingRevenue int64

	// InFlightRisk is the summed in-flight risk of this channel's pending
	// HTLCs (as an outgoing link).
	InFlightRisk uint64

	// PendingHTLCCount is the number of in-flight HTLCs for which this
	// channel is the outgoing link.
	PendingHTLCCount uint32

	// SufficientReputation is the at-rest sufficiency verdict for this
	// channel as an incoming link: whether its accrued outgoing reputation,
	// net of its current in-flight risk, already meets its incoming revenue
	// threshold (i.e. the core inequality with no additional HTLC risk).
	SufficientReputation bool
}

// Snapshot returns a read-only copy of the per-channel computed reputation
// state. It is race-free: the channel-state maps are owned exclusively by the
// worker goroutine (no mutex), so rather than reading them directly, Snapshot
// dispatches a snapshot request into the worker's event queue and blocks on the
// reply. The worker builds the snapshot while it holds sole ownership of the
// maps, preserving the single-owner, lock-free invariant.
//
// If the manager has been stopped (or is stopped while the request is in
// flight), Snapshot returns an empty slice rather than blocking forever.
func (m *Manager) Snapshot() []ChannelReputationSnapshot {
	// Buffer the reply so the worker never blocks on the send, even if this
	// goroutine has since given up (see the quit case below).
	reply := make(chan []ChannelReputationSnapshot, 1)
	req := event{kind: evSnapshot, reply: reply}

	// Dispatch the request. Use a blocking send (guarded by quit) rather
	// than the best-effort enqueue used by the hot-path hooks: a dropped
	// snapshot request would just yield an empty read, so we prefer to wait
	// for queue space. drainRemaining also serves snapshot requests already
	// queued at shutdown.
	select {
	case m.events <- req:

	case <-m.quit:
		return nil
	}

	select {
	case snaps := <-reply:
		return snaps

	case <-m.quit:
		return nil
	}
}

// buildSnapshot builds a read-only snapshot of every channel's computed state
// as of the current time, without mutating any live state. It runs only on the
// worker goroutine (invoked from process for an evSnapshot event), so it reads
// the channel-state maps without any lock. Channels are returned sorted by scid
// for deterministic output.
func (m *Manager) buildSnapshot() []ChannelReputationSnapshot {
	at := m.now()

	snaps := make([]ChannelReputationSnapshot, 0, len(m.channels))
	for scid, c := range m.channels {
		snaps = append(snaps, channelSnapshot(scid, c, at))
	}

	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].SCID < snaps[j].SCID
	})

	return snaps
}

// channelSnapshot builds a read-only snapshot of a single channel's computed
// state as of `at`, without mutating live state. Worker-only.
func channelSnapshot(scid uint64, c *channelReputation,
	at uint64) ChannelReputationSnapshot {

	outRep := c.outgoingReputation.peekAt(at)
	revenue := c.incomingRevenue.peekAt(at)
	inFlightRisk := c.outgoingInFlightRisk()

	net := saturatingAddInt64(outRep, -saturatingI64FromU64(inFlightRisk))

	return ChannelReputationSnapshot{
		SCID:                 scid,
		OutgoingReputation:   outRep,
		IncomingRevenue:      revenue,
		InFlightRisk:         inFlightRisk,
		PendingHTLCCount:     uint32(len(c.pendingHTLCs)),
		SufficientReputation: net >= revenue,
	}
}

// outgoingInFlightRisk sums the precomputed in-flight risk of every pending
// HTLC for which this channel is the outgoing link.
func (c *channelReputation) outgoingInFlightRisk() uint64 {
	var total uint64
	for _, p := range c.pendingHTLCs {
		total = saturatingAddUint64(total, p.inFlightRisk)
	}

	return total
}

// saturatingAddUint64 adds two uint64 values, clamping to MaxUint64 on overflow
// rather than wrapping.
func saturatingAddUint64(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}

	return a + b
}

// peekAt returns the value of the decaying average decayed forward to ts,
// WITHOUT mutating the stored state. It is the read-only counterpart of
// valueAt, used by Snapshot. If ts precedes the last update the stored value is
// returned unchanged (snapshots never run time backwards).
func (d *decayingAverage) peekAt(ts uint64) int64 {
	if ts < d.lastUpdated {
		return d.value
	}

	elapsed := float64(ts - d.lastUpdated)

	return clampFloatToInt64(
		math.Round(float64(d.value) * math.Pow(d.decayRate, elapsed)),
	)
}

// peekAt returns the windowed average value as of ts without mutating state. It
// mirrors valueAt (including the BOLT-proposal exponential warm-up divisor) but
// reads the inner decaying average via peekAt so the live state is untouched.
func (a *aggregatedWindowAverage) peekAt(ts uint64) int64 {
	if ts < a.start {
		return a.inner.value
	}

	warmup := a.warmupFactor(a.windowsTracked(ts))

	raw := a.inner.peekAt(ts)

	return clampFloatToInt64(math.Round(float64(raw) / warmup))
}
