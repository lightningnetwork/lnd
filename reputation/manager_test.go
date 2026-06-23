package reputation

import (
	"testing"
	"time"
)

// buildManager returns a manager seeded with two generously-sized channels
// (incoming scid=1, outgoing scid=2), an injected clock at `start`, and a fixed
// block height.
func buildManager(t *testing.T, start int64, height uint32) (*Manager,
	*testClock) {

	t.Helper()

	clock := newTestClock(start)
	src := newFakeChannelSource(
		chanInfo(1, 100, 100_000_000),
		chanInfo(2, 100, 100_000_000),
	)

	m, err := NewManager(
		DefaultConfig(),
		WithClock(clock),
		WithHeightSource(func() uint32 { return height }),
		WithChannelSource(src),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	return m, clock
}

// TestForwardSettleLifecycle exercises P2 (pending lifecycle) + P4 (reputation
// accrual): an unaccountable HTLC that settles quickly earns its fee.
func TestForwardSettleLifecycle(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clock := buildManager(t, start, 100)

	in := circuit(1, 0)
	out := circuit(2, 0)

	// fee = 2000 - 1000 = 1000. cltv 200 > height 100.
	m.OnForward(in, out, 2000, 1000, 200, false)

	m.mu.Lock()
	outChan := m.channels[2]
	if len(outChan.pendingHTLCs) != 1 {
		m.mu.Unlock()
		t.Fatalf("expected 1 pending, got %d",
			len(outChan.pendingHTLCs))
	}
	m.mu.Unlock()

	// Settle 30s later (within resolution period).
	clock.advance(30 * time.Second)
	m.OnSettle(in, out)

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(outChan.pendingHTLCs) != 0 {
		t.Fatalf("pending not cleared: %d", len(outChan.pendingHTLCs))
	}

	rep, err := outChan.outgoingReputation.valueAt(m.now())
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}
	if rep != 1000 {
		t.Fatalf("reputation: got %d, want 1000", rep)
	}

	// Incoming channel earned the fee as revenue.
	inChan := m.channels[1]
	rev, err := inChan.incomingRevenue.valueAt(m.now())
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}
	if rev <= 0 {
		t.Fatalf("revenue: got %d, want > 0", rev)
	}
}

// TestFailDoesNotEarnRevenue checks that a failed unaccountable HTLC neither
// helps reputation nor adds revenue.
func TestFailDoesNotEarnRevenue(t *testing.T) {
	t.Parallel()

	m, clock := buildManager(t, 1_000_000, 100)
	in, out := circuit(1, 0), circuit(2, 0)

	m.OnForward(in, out, 2000, 1000, 200, false)
	clock.advance(30 * time.Second)
	m.OnFail(in, out)

	m.mu.Lock()
	defer m.mu.Unlock()

	rep, _ := m.channels[2].outgoingReputation.valueAt(m.now())
	if rep != 0 {
		t.Fatalf("reputation after fail: got %d, want 0", rep)
	}
	rev, _ := m.channels[1].incomingRevenue.valueAt(m.now())
	if rev != 0 {
		t.Fatalf("revenue after fail: got %d, want 0", rev)
	}
}

// TestUnmatchedResolveNoop ensures a resolve with no matching forward is a safe
// no-op (tolerating a missed add / mid-flight enable).
func TestUnmatchedResolveNoop(t *testing.T) {
	t.Parallel()

	m, _ := buildManager(t, 1_000_000, 100)

	// Should not panic or error fatally.
	m.OnSettle(circuit(1, 99), circuit(2, 99))
	m.OnFail(circuit(1, 88), circuit(2, 88))
}

// TestGCStalePending verifies stale pendings are evicted past their max hold.
func TestGCStalePending(t *testing.T) {
	t.Parallel()

	m, clock := buildManager(t, 1_000_000, 100)
	in, out := circuit(1, 0), circuit(2, 0)

	// cltv 101 at height 100 => max hold 600s.
	m.OnForward(in, out, 2000, 1000, 101, false)

	// Advance past the max hold and run GC.
	clock.advance(700 * time.Second)
	m.gcStalePendings()

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.channels[2].pendingHTLCs) != 0 {
		t.Fatalf("stale pending not evicted")
	}
}

// TestSufficiencyBoundary unit-tests the core inequality at its boundary.
func TestSufficiencyBoundary(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	const start = 1_000_000
	c := newChannelReputation(1, 100_000_000, 100, cfg, start)

	// Seed an incoming-revenue threshold of 1000 and read it back so the
	// aggregated average's warmup divisor is settled.
	if _, err := c.incomingRevenue.add(1000, start); err != nil {
		t.Fatalf("add: %v", err)
	}
	threshold, err := c.incomingRevenue.valueAt(start)
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}

	// Reputation exactly at threshold, no in-flight risk => sufficient.
	ok, _, err := c.sufficientReputation(0, threshold, 0, start)
	if err != nil {
		t.Fatalf("sufficientReputation: %v", err)
	}
	if !ok {
		t.Fatalf("expected sufficient at threshold")
	}

	// One msat below threshold => insufficient.
	ok, _, err = c.sufficientReputation(0, threshold-1, 0, start)
	if err != nil {
		t.Fatalf("sufficientReputation: %v", err)
	}
	if ok {
		t.Fatalf("expected insufficient below threshold")
	}

	// At threshold but with in-flight risk => insufficient.
	ok, _, err = c.sufficientReputation(1, threshold, 0, start)
	if err != nil {
		t.Fatalf("sufficientReputation: %v", err)
	}
	if ok {
		t.Fatalf("expected insufficient with in-flight risk")
	}
}

// TestDecisionTreeBranches exercises the key decision-tree outcomes via the
// white-box addHTLCLocked path.
func TestDecisionTreeBranches(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	height := uint32(100)

	t.Run("unaccountable uses general", func(t *testing.T) {
		m, _ := buildManager(t, start, height)
		m.mu.Lock()
		defer m.mu.Unlock()

		o, err := m.addHTLCLocked(
			circuit(1, 0), circuit(2, 0), 2000, 1000, 200, false,
			uint64(start), height,
		)
		if err != nil {
			t.Fatalf("addHTLCLocked: %v", err)
		}
		if !o.forward || o.bucket != bucketGeneral || o.accountable {
			t.Fatalf("unexpected outcome: %s", o)
		}
	})

	t.Run("accountable insufficient fails", func(t *testing.T) {
		m, _ := buildManager(t, start, height)
		m.mu.Lock()
		defer m.mu.Unlock()

		// Give the incoming channel a high revenue threshold so the
		// (zero) reputation is insufficient.
		inChan := m.channels[1]
		_, _ = inChan.incomingRevenue.add(1_000_000, uint64(start))

		o, err := m.addHTLCLocked(
			circuit(1, 0), circuit(2, 0), 2000, 1000, 200, true,
			uint64(start), height,
		)
		if err != nil {
			t.Fatalf("addHTLCLocked: %v", err)
		}
		if o.forward {
			t.Fatalf("expected FAIL, got %s", o)
		}
	})

	t.Run("accountable sufficient uses protected", func(t *testing.T) {
		m, _ := buildManager(t, start, height)
		m.mu.Lock()
		defer m.mu.Unlock()

		// Give the outgoing channel ample reputation.
		outChan := m.channels[2]
		_, _ = outChan.outgoingReputation.add(10_000_000, uint64(start))

		o, err := m.addHTLCLocked(
			circuit(1, 0), circuit(2, 0), 2000, 1000, 200, true,
			uint64(start), height,
		)
		if err != nil {
			t.Fatalf("addHTLCLocked: %v", err)
		}
		if !o.forward || o.bucket != bucketProtected || !o.accountable {
			t.Fatalf("unexpected outcome: %s", o)
		}
	})
}

// TestCongestionMisuseWindow checks the ~2-week congestion-misuse block.
func TestCongestionMisuseWindow(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	const start = 1_000_000
	c := newChannelReputation(1, 100_000_000, 100, cfg, start)

	// No misuse initially.
	misused, err := c.hasMisusedCongestion(2, start)
	if err != nil || misused {
		t.Fatalf("unexpected initial misuse: %v %v", misused, err)
	}

	// Record misuse, then check it blocks within two weeks and clears
	// afterwards.
	c.misusedCongestion(2, start)

	misused, err = c.hasMisusedCongestion(2, start+60)
	if err != nil || !misused {
		t.Fatalf("expected misuse within window: %v %v", misused, err)
	}

	afterTwoWeeks := uint64(start) + twoWeeksSeconds + 1
	misused, err = c.hasMisusedCongestion(2, afterTwoWeeks)
	if err != nil || misused {
		t.Fatalf("expected misuse cleared after window: %v %v",
			misused, err)
	}
}

// TestChannelEventRemovesState verifies a close event drops channel state.
func TestChannelEventRemovesState(t *testing.T) {
	t.Parallel()

	m, _ := buildManager(t, 1_000_000, 100)

	m.handleChannelEvent(ChannelEvent{
		Info: chanInfo(2, 100, 100_000_000),
		Open: false,
	})

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.channels[2]; ok {
		t.Fatalf("channel 2 not removed")
	}
}
