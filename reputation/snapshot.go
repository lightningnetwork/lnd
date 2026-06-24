package reputation

import (
	"math"
	"sort"
)

// BucketOccupancy reports the live occupancy of a single resource bucket: how
// many slots and how much liquidity are currently in use out of the amounts
// allocated to it.
type BucketOccupancy struct {
	// SlotsUsed is the number of slots currently occupied.
	SlotsUsed uint16

	// SlotsAllocated is the total number of slots assigned to the bucket.
	SlotsAllocated uint16

	// LiquidityUsedMsat is the millisatoshi liquidity currently reserved.
	LiquidityUsedMsat uint64

	// LiquidityAllocatedMsat is the total millisatoshi liquidity assigned
	// to the bucket.
	LiquidityAllocatedMsat uint64
}

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
	// accountable HTLCs (as an outgoing link).
	InFlightRisk uint64

	// PendingHTLCCount is the number of in-flight HTLCs for which this
	// channel is the outgoing link.
	PendingHTLCCount uint32

	// SufficientReputation is the at-rest sufficiency verdict for this
	// channel as an incoming link: whether its accrued outgoing reputation,
	// net of its current in-flight risk, already meets its incoming revenue
	// threshold (i.e. the core inequality with no additional HTLC risk).
	SufficientReputation bool

	// General/Congestion/Protected report the live occupancy of this
	// channel's resource buckets (which it owns as an incoming link).
	General    BucketOccupancy
	Congestion BucketOccupancy
	Protected  BucketOccupancy
}

// Snapshot returns a read-only copy of the per-channel computed reputation
// state. It is a pure read: it takes the manager lock and computes decayed
// values without mutating any live state, mirroring the snapshot-under-lock
// pattern used by the persistence flush. Channels are returned sorted by scid
// for deterministic output.
func (m *Manager) Snapshot() []ChannelReputationSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	at := m.now()

	snaps := make([]ChannelReputationSnapshot, 0, len(m.channels))
	for scid, c := range m.channels {
		snaps = append(snaps, channelSnapshotLocked(scid, c, at))
	}

	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].SCID < snaps[j].SCID
	})

	return snaps
}

// channelSnapshotLocked builds a read-only snapshot of a single channel's
// computed state as of `at`, without mutating live state. Caller must hold mu.
func channelSnapshotLocked(scid uint64, c *channelReputation,
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
		General: BucketOccupancy{
			SlotsUsed:              c.generalBucket.slotsUsed(),
			SlotsAllocated:         c.generalBucket.totalSlots,
			LiquidityUsedMsat:      c.generalBucket.liquidityUsed(),
			LiquidityAllocatedMsat: c.generalBucket.totalLiquidity,
		},
		Congestion: bucketOccupancy(c.congestionBucket),
		Protected:  bucketOccupancy(c.protectedBucket),
	}
}

// slotsUsed returns the number of currently occupied general-bucket slots.
func (g *generalBucket) slotsUsed() uint16 {
	var used uint16
	for _, occ := range g.slotsOccupied {
		if occ != nil {
			used++
		}
	}

	return used
}

// liquidityUsed returns the liquidity currently reserved in the general bucket,
// derived from the number of occupied slots and the per-slot liquidity.
func (g *generalBucket) liquidityUsed() uint64 {
	return uint64(g.slotsUsed()) * g.perSlotMsat
}

// bucketOccupancy reads the occupancy of a simple slot+liquidity bucket.
func bucketOccupancy(b *bucketResources) BucketOccupancy {
	return BucketOccupancy{
		SlotsUsed:              b.slotsUsed,
		SlotsAllocated:         b.slotsAllocated,
		LiquidityUsedMsat:      b.liquidityUsed,
		LiquidityAllocatedMsat: b.liquidityAllocated,
	}
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

// peekAt returns the windowed average value as of ts without mutating state.
func (a *aggregatedWindowAverage) peekAt(ts uint64) int64 {
	if ts < a.start {
		return a.inner.value
	}

	tracked := a.windowsTracked(ts)
	if tracked < 1.0 {
		tracked = 1.0
	}

	divisor := math.Min(tracked, float64(a.windowCount))

	raw := a.inner.peekAt(ts)

	return clampFloatToInt64(math.Round(float64(raw) / divisor))
}
