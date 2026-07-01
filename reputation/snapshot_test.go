package reputation

import (
	"testing"
	"time"
)

// TestSnapshotForwardSettle verifies that Snapshot reflects the exact computed
// state after a forward+settle: the outgoing channel gains exactly the fee as
// reputation, the incoming channel records the fee as revenue, bucket occupancy
// returns to zero once the HTLC resolves, and the read is non-mutating.
func TestSnapshotForwardSettle(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clock := buildManager(t, start, 100)

	in, out := circuit(1, 0), circuit(2, 0)

	// fee = 2000 - 1000 = 1000. cltv 200 > height 100. Unaccountable so it
	// lands in the incoming channel's general bucket.
	m.OnForward(in, out, 2000, 1000, 200, false)

	// While in flight, the incoming channel (scid 1) must show
	// general-bucket occupancy and the outgoing channel (scid 2) must show
	// a pending HTLC.
	snaps := m.Snapshot()
	if len(snaps) != 2 {
		t.Fatalf("snapshot channels: got %d, want 2", len(snaps))
	}
	inSnap := snapForSCID(t, snaps, 1)
	outSnap := snapForSCID(t, snaps, 2)

	if inSnap.General.SlotsUsed == 0 {
		t.Fatalf("incoming general slots used: got 0, want > 0")
	}
	if inSnap.General.LiquidityUsedMsat == 0 {
		t.Fatalf("incoming general liquidity used: got 0, want > 0")
	}
	if outSnap.PendingHTLCCount != 1 {
		t.Fatalf("outgoing pending: got %d, want 1",
			outSnap.PendingHTLCCount)
	}

	// Snapshot must be a pure read: calling it again yields identical
	// values (no decay applied to live state, since time has not advanced).
	snaps2 := m.Snapshot()
	if snapForSCID(t, snaps2, 1).General.SlotsUsed !=
		inSnap.General.SlotsUsed {

		t.Fatalf("snapshot mutated live bucket state")
	}

	// Settle 30s later (within the resolution period).
	clock.advance(30 * time.Second)
	m.OnSettle(in, out)

	snaps = m.Snapshot()
	inSnap = snapForSCID(t, snaps, 1)
	outSnap = snapForSCID(t, snaps, 2)

	// The outgoing channel gained exactly the 1000 msat fee as reputation.
	if outSnap.OutgoingReputation != 1000 {
		t.Fatalf("outgoing reputation: got %d, want 1000",
			outSnap.OutgoingReputation)
	}

	// The incoming channel recorded the fee as revenue (positive).
	if inSnap.IncomingRevenue <= 0 {
		t.Fatalf("incoming revenue: got %d, want > 0",
			inSnap.IncomingRevenue)
	}

	// Bucket occupancy returned to zero across all buckets after settle.
	for _, s := range snaps {
		gen := s.General
		if gen.SlotsUsed != 0 || gen.LiquidityUsedMsat != 0 {
			t.Fatalf("scid %d general not released: %+v", s.SCID,
				s.General)
		}
		if s.Congestion.SlotsUsed != 0 || s.Protected.SlotsUsed != 0 {
			t.Fatalf("scid %d congestion/protected not released",
				s.SCID)
		}
		if s.PendingHTLCCount != 0 {
			t.Fatalf("scid %d pending not cleared: %d", s.SCID,
				s.PendingHTLCCount)
		}
		if s.InFlightRisk != 0 {
			t.Fatalf("scid %d in-flight risk not cleared: %d",
				s.SCID, s.InFlightRisk)
		}
	}

	// Allocations are reported and consistent with the channel sizing.
	if inSnap.General.SlotsAllocated == 0 {
		t.Fatalf("incoming general slots allocated: got 0, want > 0")
	}
}

// TestSnapshotEmpty verifies Snapshot on a freshly-seeded manager reports zero
// computed state for each channel.
func TestSnapshotEmpty(t *testing.T) {
	t.Parallel()

	m, _ := buildManager(t, 1_000_000, 100)

	for _, s := range m.Snapshot() {
		if s.OutgoingReputation != 0 || s.IncomingRevenue != 0 {
			t.Fatalf("scid %d non-zero at rest: rep=%d rev=%d",
				s.SCID, s.OutgoingReputation, s.IncomingRevenue)
		}
		if s.PendingHTLCCount != 0 || s.InFlightRisk != 0 {
			t.Fatalf("scid %d non-zero pending state", s.SCID)
		}
		// At rest, reputation (0) meets the revenue threshold (0).
		if !s.SufficientReputation {
			t.Fatalf("scid %d expected sufficient at rest", s.SCID)
		}
	}
}

// TestSnapshotPeekIsNonMutating asserts the peekAt helpers do not advance the
// underlying decaying averages, unlike valueAt.
func TestSnapshotPeekIsNonMutating(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	c := newChannelReputation(1, 100_000_000, 100, DefaultConfig(), start)

	if _, err := c.outgoingReputation.add(1000, start); err != nil {
		t.Fatalf("add: %v", err)
	}

	before := c.outgoingReputation.lastUpdated

	// Peek far into the future; this must not advance lastUpdated.
	later := uint64(start) + uint64((30 * 24 * time.Hour).Seconds())
	_ = c.outgoingReputation.peekAt(later)

	if c.outgoingReputation.lastUpdated != before {
		t.Fatalf("peekAt mutated lastUpdated: %d -> %d", before,
			c.outgoingReputation.lastUpdated)
	}
}

// snapForSCID returns the snapshot for the given scid, failing if absent.
func snapForSCID(t *testing.T, snaps []ChannelReputationSnapshot,
	scid uint64) ChannelReputationSnapshot {

	t.Helper()

	for _, s := range snaps {
		if s.SCID == scid {
			return s
		}
	}

	t.Fatalf("no snapshot for scid %d", scid)

	return ChannelReputationSnapshot{}
}
