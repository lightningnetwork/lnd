package reputation

import (
	"errors"
	"testing"
	"time"
)

// occupiedGeneralSlots counts the occupied slots in a general bucket.
func occupiedGeneralSlots(g *generalBucket) int {
	var n int
	for _, s := range g.slotsOccupied {
		if s != nil {
			n++
		}
	}

	return n
}

// fillGeneralBucket occupies all of an outgoing scid's assigned general-bucket
// slots so that canAddHTLC returns false for it.
func fillGeneralBucket(t *testing.T, inChan *channelReputation,
	outScid uint64) {

	t.Helper()

	for i := 0; i < int(inChan.generalBucket.perChannelSlots); i++ {
		if err := inChan.generalBucket.addHTLC(outScid, 1); err != nil {
			t.Fatalf("fill general: %v", err)
		}
	}

	ok, err := inChan.generalBucket.canAddHTLC(outScid, 1)
	if err != nil {
		t.Fatalf("canAddHTLC: %v", err)
	}
	if ok {
		t.Fatalf("general bucket not full after fill")
	}
}

// TestBucketRoundTripPublicPath drives OnForward -> OnSettle/OnFail through the
// public hooks and asserts bucket occupancy is reserved on forward and fully
// released on resolution (review finding T2; the occupancy half of the event
// loop was previously untested through the public path).
func TestBucketRoundTripPublicPath(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, _ := buildManager(t, start, 100)

	// Unaccountable forward -> general bucket on the incoming channel (1).
	in, out := circuit(1, 0), circuit(2, 0)
	m.OnForward(in, out, 2000, 1000, 200, false)

	m.mu.Lock()
	inChan := m.channels[1]
	if got := occupiedGeneralSlots(inChan.generalBucket); got != 1 {
		m.mu.Unlock()
		t.Fatalf("general occupancy after forward: got %d, want 1", got)
	}
	m.mu.Unlock()

	m.OnSettle(in, out)

	m.mu.Lock()
	if got := occupiedGeneralSlots(inChan.generalBucket); got != 0 {
		m.mu.Unlock()
		t.Fatalf("general occupancy after settle: got %d, want 0", got)
	}
	m.mu.Unlock()

	// Accountable forward with ample reputation -> protected bucket.
	m.mu.Lock()
	if _, err := m.channels[2].outgoingReputation.add(
		10_000_000, start,
	); err != nil {
		m.mu.Unlock()
		t.Fatalf("seed reputation: %v", err)
	}
	m.mu.Unlock()

	in2, out2 := circuit(1, 1), circuit(2, 1)
	m.OnForward(in2, out2, 2000, 1000, 200, true)

	m.mu.Lock()
	if inChan.protectedBucket.slotsUsed != 1 ||
		inChan.protectedBucket.liquidityUsed != 2000 {

		m.mu.Unlock()
		t.Fatalf("protected occupancy after forward: %+v",
			inChan.protectedBucket)
	}
	m.mu.Unlock()

	m.OnFail(in2, out2)

	m.mu.Lock()
	defer m.mu.Unlock()
	if inChan.protectedBucket.slotsUsed != 0 ||
		inChan.protectedBucket.liquidityUsed != 0 {

		t.Fatalf("protected occupancy after fail: %+v",
			inChan.protectedBucket)
	}
}

// TestGCReleasesBuckets verifies the stale-pending GC releases the bucket
// resources the pending reserved, rather than leaking them (review finding B2).
func TestGCReleasesBuckets(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clock := buildManager(t, start, 100)

	// cltv 102 at height 100 => max hold = 2 * 600 = 1200s.
	in, out := circuit(1, 0), circuit(2, 0)
	m.OnForward(in, out, 2000, 1000, 102, false)

	m.mu.Lock()
	inChan := m.channels[1]
	if occupiedGeneralSlots(inChan.generalBucket) != 1 {
		m.mu.Unlock()
		t.Fatalf("expected 1 occupied general slot after forward")
	}
	m.mu.Unlock()

	// Advance past the worst-case hold and run GC.
	clock.advance(2000 * time.Second)
	m.gcStalePendings()

	m.mu.Lock()
	defer m.mu.Unlock()

	if got := occupiedGeneralSlots(inChan.generalBucket); got != 0 {
		t.Fatalf("GC leaked general occupancy: got %d, want 0 (B2)",
			got)
	}
	if n := len(m.channels[2].pendingHTLCs); n != 0 {
		t.Fatalf("GC did not evict pending: %d remain", n)
	}
}

// TestGCStampsCongestionMisuse verifies that a congestion-bucket HTLC
// evicted by the stale-pending GC records congestion misuse, mirroring
// the resolve path: a GC eviction means the HTLC was held past its
// worst-case hold time, the strongest misuse signal (review finding
// m-1).
func TestGCStampsCongestionMisuse(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clock := buildManager(t, start, 100)

	// Saturate the incoming channel's general bucket for the outgoing scid
	// and leave reputation at zero, so the forward is routed to congestion.
	m.mu.Lock()
	inChan := m.channels[1]
	fillGeneralBucket(t, inChan, 2)
	m.mu.Unlock()

	in, out := circuit(1, 0), circuit(2, 0)
	m.OnForward(in, out, 2000, 1000, 102, false)

	m.mu.Lock()
	p, ok := m.channels[2].pendingHTLCs[in]
	if !ok || p.bucket != bucketCongestion {
		m.mu.Unlock()
		t.Fatalf("expected pending in congestion bucket (ok=%v)", ok)
	}
	m.mu.Unlock()

	// Advance past the worst-case hold (cltv 102 - height 100 => 1200s) and
	// run GC; the evicted congestion HTLC must record misuse.
	clock.advance(2000 * time.Second)
	m.gcStalePendings()

	m.mu.Lock()
	defer m.mu.Unlock()

	misused, err := inChan.hasMisusedCongestion(2, m.now())
	if err != nil {
		t.Fatalf("hasMisusedCongestion: %v", err)
	}
	if !misused {
		t.Fatalf("GC did not record congestion misuse (m-1)")
	}
}

// TestDecisionTreeRemainingLeaves covers the decision-tree leaves not exercised
// by TestDecisionTreeBranches: general-full fallbacks to protected/congestion
// and protected-full spillover to general (review finding T3).
func TestDecisionTreeRemainingLeaves(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	height := uint32(100)

	t.Run("general-full sufficient->protected", func(t *testing.T) {
		m, _ := buildManager(t, start, height)
		m.mu.Lock()
		defer m.mu.Unlock()

		inChan := m.channels[1]
		fillGeneralBucket(t, inChan, 2)
		if _, err := m.channels[2].outgoingReputation.add(
			10_000_000, start,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}

		o, err := m.addHTLCLocked(
			circuit(1, 0), circuit(2, 0), 2000, 1000, 200, false,
			uint64(start), height,
		)
		if err != nil {
			t.Fatalf("addHTLCLocked: %v", err)
		}
		if !o.forward || o.bucket != bucketProtected {
			t.Fatalf("unexpected outcome: %s", o)
		}
	})

	t.Run("general-full insufficient->congestion", func(t *testing.T) {
		m, _ := buildManager(t, start, height)
		m.mu.Lock()
		defer m.mu.Unlock()

		inChan := m.channels[1]
		fillGeneralBucket(t, inChan, 2)

		o, err := m.addHTLCLocked(
			circuit(1, 0), circuit(2, 0), 2000, 1000, 200, false,
			uint64(start), height,
		)
		if err != nil {
			t.Fatalf("addHTLCLocked: %v", err)
		}
		if !o.forward || o.bucket != bucketCongestion ||
			!o.accountable {

			t.Fatalf("unexpected outcome: %s", o)
		}
	})

	t.Run("protected-full sufficient->general", func(t *testing.T) {
		m, _ := buildManager(t, start, height)
		m.mu.Lock()
		defer m.mu.Unlock()

		inChan := m.channels[1]
		// Saturate the protected bucket.
		inChan.protectedBucket.slotsUsed =
			inChan.protectedBucket.slotsAllocated
		inChan.protectedBucket.liquidityUsed =
			inChan.protectedBucket.liquidityAllocated

		if _, err := m.channels[2].outgoingReputation.add(
			10_000_000, start,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}

		o, err := m.addHTLCLocked(
			circuit(1, 0), circuit(2, 0), 2000, 1000, 200, true,
			uint64(start), height,
		)
		if err != nil {
			t.Fatalf("addHTLCLocked: %v", err)
		}
		if !o.forward || o.bucket != bucketGeneral || !o.accountable {
			t.Fatalf("unexpected outcome: %s", o)
		}
	})
}

// TestCongestionMisuseViaResolve drives a congestion-bucket HTLC through the
// public path and verifies that resolving it slowly stamps congestion misuse,
// blocking the outgoing scid from the congestion bucket (review finding T3).
func TestCongestionMisuseViaResolve(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clock := buildManager(t, start, 100)

	// Force the next unaccountable forward into the congestion bucket by
	// filling general (and leaving reputation at zero).
	m.mu.Lock()
	inChan := m.channels[1]
	fillGeneralBucket(t, inChan, 2)
	m.mu.Unlock()

	in, out := circuit(1, 0), circuit(2, 0)
	m.OnForward(in, out, 2000, 1000, 200, false)

	m.mu.Lock()
	if inChan.congestionBucket.slotsUsed != 1 {
		m.mu.Unlock()
		t.Fatalf("expected HTLC in congestion bucket")
	}
	m.mu.Unlock()

	// Resolve well past the resolution period -> congestion misuse.
	clock.advance(200 * time.Second)
	m.OnSettle(in, out)

	m.mu.Lock()
	defer m.mu.Unlock()

	misused, err := inChan.hasMisusedCongestion(2, m.now())
	if err != nil {
		t.Fatalf("hasMisusedCongestion: %v", err)
	}
	if !misused {
		t.Fatalf("expected congestion misuse to be recorded")
	}
}

// TestColdStartZeroState verifies that enabling the subsystem with no
// persisted state seeds the active channels with zero reputation
// (review finding T3 / P9).
func TestColdStartZeroState(t *testing.T) {
	t.Parallel()

	clock := newTestClock(1_000_000)
	src := newFakeChannelSource(
		chanInfo(7, 50, 50_000_000),
		chanInfo(8, 50, 50_000_000),
	)

	m, err := NewManager(
		DefaultConfig(),
		WithClock(clock),
		WithHeightSource(func() uint32 { return 100 }),
		WithChannelSource(src),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, scid := range []uint64{7, 8} {
		c, ok := m.channels[scid]
		if !ok {
			t.Fatalf("channel %d not seeded on cold start", scid)
		}
		rep, err := c.outgoingReputation.valueAt(m.now())
		if err != nil {
			t.Fatalf("valueAt: %v", err)
		}
		if rep != 0 {
			t.Fatalf("cold-start reputation not zero: %d", rep)
		}
	}
}

// failingStore is a Store whose PersistChannels always fails, used to verify
// the dirty set is retried rather than dropped.
type failingStore struct{}

func (failingStore) LoadChannels() ([]ChannelSnapshot, error) {
	return nil, nil
}

func (failingStore) PersistChannels([]ChannelSnapshot) error {
	return errors.New("persist failed")
}

// TestFlushRetainsDirtyOnError verifies a failed persistence flush re-marks the
// channels as dirty so they are retried, rather than silently dropping them
// (review finding B3).
func TestFlushRetainsDirtyOnError(t *testing.T) {
	t.Parallel()

	clock := newTestClock(1_000_000)
	src := newFakeChannelSource(
		chanInfo(1, 100, 100_000_000),
		chanInfo(2, 100, 100_000_000),
	)

	m, err := NewManager(
		DefaultConfig(),
		WithClock(clock),
		WithHeightSource(func() uint32 { return 100 }),
		WithChannelSource(src),
		WithStore(failingStore{}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	// Produce dirty state.
	m.OnForward(circuit(1, 0), circuit(2, 0), 2000, 1000, 200, false)

	if err := m.flush(); err == nil {
		t.Fatalf("expected flush error from failing store")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.dirty) == 0 {
		t.Fatalf("dirty set dropped on failed flush (B3)")
	}
}
