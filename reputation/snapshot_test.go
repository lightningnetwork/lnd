package reputation

import (
	"testing"
	"time"
)

// TestSnapshotForwardSettle verifies that Snapshot reflects the exact computed
// state after a forward+settle: the outgoing channel gains exactly the fee as
// reputation, the incoming channel records the fee as revenue, pending state
// returns to zero once the HTLC resolves, and the read is non-mutating. The
// snapshot is served by the worker goroutine (the sole owner of the state
// maps), so this also exercises the actor-request path.
func TestSnapshotForwardSettle(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clock := buildManager(t, start, 100)

	in, out := circuit(1, 0), circuit(2, 0)

	// advertised fee = 1000. cltv 200 > height 100. Unaccountable.
	m.OnForward(in, out, 2000, 1000, 1000, 200, false)
	m.sync()

	// While in flight, the outgoing channel (scid 2) must show a pending
	// HTLC with non-zero in-flight risk, and both channels must be present.
	snaps := m.Snapshot()
	if len(snaps) != 2 {
		t.Fatalf("snapshot channels: got %d, want 2", len(snaps))
	}
	outSnap := snapForSCID(t, snaps, 2)

	if outSnap.PendingHTLCCount != 1 {
		t.Fatalf("outgoing pending: got %d, want 1",
			outSnap.PendingHTLCCount)
	}

	// Snapshot must be a pure read: calling it again yields identical
	// values (no decay applied to live state, since time has not advanced).
	snaps2 := m.Snapshot()
	if snapForSCID(t, snaps2, 2).PendingHTLCCount !=
		outSnap.PendingHTLCCount {

		t.Fatalf("snapshot mutated live pending state")
	}

	// Settle 30s later (within the resolution period).
	clock.advance(30 * time.Second)
	m.OnSettle(in, out)
	m.sync()

	snaps = m.Snapshot()
	inSnap := snapForSCID(t, snaps, 1)
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

	// Pending state returned to zero across all channels after settle.
	for _, s := range snaps {
		if s.PendingHTLCCount != 0 {
			t.Fatalf("scid %d pending not cleared: %d", s.SCID,
				s.PendingHTLCCount)
		}
		if s.InFlightRisk != 0 {
			t.Fatalf("scid %d in-flight risk not cleared: %d",
				s.SCID, s.InFlightRisk)
		}
	}
}

// TestSnapshotEmpty verifies Snapshot on a manager with no observed traffic
// reports no channels.
func TestSnapshotEmpty(t *testing.T) {
	t.Parallel()

	m, _ := buildManager(t, 1_000_000, 100)

	if snaps := m.Snapshot(); len(snaps) != 0 {
		t.Fatalf("snapshot on idle manager: got %d channels, want 0",
			len(snaps))
	}
}

// TestSnapshotSufficiency verifies the at-rest sufficiency verdict on a channel
// as an incoming link: it holds only when the channel's own outgoing
// reputation, net of zero in-flight risk, meets its incoming revenue threshold.
// A forward+settle earns the outgoing channel (scid 2) reputation but no
// revenue (revenue accrues to the incoming channel, scid 1), so scid 2 is
// sufficient (rep 1000 >= revenue 0) while scid 1 is not (rep 0 < revenue > 0).
func TestSnapshotSufficiency(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clock := buildManager(t, start, 100)

	in, out := circuit(1, 0), circuit(2, 0)
	m.OnForward(in, out, 2000, 1000, 1000, 200, false)
	clock.advance(30 * time.Second)
	m.OnSettle(in, out)
	m.sync()

	snaps := m.Snapshot()
	for _, s := range snaps {
		if s.PendingHTLCCount != 0 || s.InFlightRisk != 0 {
			t.Fatalf("scid %d non-zero pending state", s.SCID)
		}
	}

	if !snapForSCID(t, snaps, 2).SufficientReputation {
		t.Fatalf("scid 2 expected sufficient (rep >= 0 revenue)")
	}
	if snapForSCID(t, snaps, 1).SufficientReputation {
		t.Fatalf("scid 1 expected insufficient (0 rep < positive " +
			"revenue)")
	}
}

// TestSnapshotStopped verifies Snapshot does not block once the manager has
// been stopped: the worker is gone, so the request cannot be served and
// Snapshot returns nil rather than deadlocking.
func TestSnapshotStopped(t *testing.T) {
	t.Parallel()

	m, err := NewManager(
		DefaultConfig(),
		WithClock(newTestClock(1_000_000)),
		WithHeightSource(func() (uint32, error) { return 100, nil }),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if snaps := m.Snapshot(); snaps != nil {
		t.Fatalf("snapshot after stop: got %v, want nil", snaps)
	}
}

// TestSnapshotPeekIsNonMutating asserts the peekAt helpers do not advance the
// underlying decaying averages, unlike valueAt.
func TestSnapshotPeekIsNonMutating(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	c := newChannelReputation(DefaultConfig(), start)

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
