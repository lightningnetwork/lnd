package reputation

import (
	"errors"
	"testing"
	"time"
)

// TestRefreshHeightKeepsCachedOnError verifies that a height-source error does
// not clobber the cached height with zero: the manager keeps the last good
// value (finding #5).
func TestRefreshHeightKeepsCachedOnError(t *testing.T) {
	t.Parallel()

	var fail bool
	m, err := NewManager(
		DefaultConfig(),
		WithClock(newTestClock(1000)),
		WithHeightSource(func() (uint32, error) {
			if fail {
				return 0, errors.New("rpc down")
			}

			return 850_000, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// The constructor seeded the cached height from the good source.
	if got := m.cachedHeight.Load(); got != 850_000 {
		t.Fatalf("seeded height: got %d, want 850000", got)
	}

	// A subsequent failing refresh must keep the cached value, not zero it.
	fail = true
	m.refreshHeight()
	if got := m.cachedHeight.Load(); got != 850_000 {
		t.Fatalf("height after failed refresh: got %d, want 850k", got)
	}
}

// buildManager returns a started manager with an injected clock at `start` and
// a fixed block height. Per-channel state (incoming scid=1, outgoing scid=2) is
// created lazily on the first HTLC event.
func buildManager(t *testing.T, start int64, height uint32) (*Manager,
	*testClock) {

	t.Helper()

	clock := newTestClock(start)

	m, err := NewManager(
		DefaultConfig(),
		WithClock(clock),
		WithHeightSource(func() (uint32, error) { return height, nil }),
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

// buildManagerNoStart returns a manager whose worker is NOT started, for
// white-box tests that call the worker-only methods (addHTLC, gcStalePendings)
// directly on the test goroutine — safe precisely because no worker is running
// to race with.
func buildManagerNoStart(t *testing.T, start int64, height uint32) *Manager {
	t.Helper()

	m, err := NewManager(
		DefaultConfig(),
		WithClock(newTestClock(start)),
		WithHeightSource(func() (uint32, error) { return height, nil }),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	return m
}

// TestForwardSettleLifecycle exercises the pending lifecycle + reputation
// accrual: an unaccountable HTLC that settles quickly earns its fee.
func TestForwardSettleLifecycle(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clock := buildManager(t, start, 100)

	in := circuit(1, 0)
	out := circuit(2, 0)

	// advertised fee = 1000 (equals in-out here). cltv 200 > height 100.
	m.OnForward(in, out, 2000, 1000, 1000, 200, false)
	m.sync()

	outChan := m.channels[2]
	if len(outChan.pendingHTLCs) != 1 {
		t.Fatalf("expected 1 pending, got %d",
			len(outChan.pendingHTLCs))
	}

	// Settle 30s later (within resolution period).
	clock.advance(30 * time.Second)
	m.OnSettle(in, out)
	m.sync()

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

	m.OnForward(in, out, 2000, 1000, 1000, 200, false)
	clock.advance(30 * time.Second)
	m.OnFail(in, out)
	m.sync()

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
	m.sync()
}

// TestGCStalePending verifies stale pendings are evicted past their max hold.
// It drives the worker-only add + GC paths directly (no running worker) so the
// map is exercised deterministically on a single goroutine.
func TestGCStalePending(t *testing.T) {
	t.Parallel()

	m := buildManagerNoStart(t, 1_000_000, 100)

	// cltv 101 at height 100 => max hold 600s.
	if _, err := m.addHTLC(event{
		incoming:      circuit(1, 0),
		outgoing:      circuit(2, 0),
		incomingAmt:   2000,
		outgoingAmt:   1000,
		advertisedFee: 1000,
		incomingCltv:  101,
		at:            1_000_000,
		height:        100,
	}); err != nil {
		t.Fatalf("addHTLC: %v", err)
	}

	// Advance past the max hold and run GC.
	clock, ok := m.clock.(*testClock)
	if !ok {
		t.Fatalf("expected testClock")
	}
	clock.advance(700 * time.Second)
	m.gcStalePendings()

	if len(m.channels[2].pendingHTLCs) != 0 {
		t.Fatalf("stale pending not evicted")
	}
}

// TestSufficiencyBoundary unit-tests the core isolation inequality at its
// boundary.
func TestSufficiencyBoundary(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	const start = 1_000_000
	c := newChannelReputation(cfg, start)

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
	ok, _, err := c.sufficientReputation(0, threshold, start)
	if err != nil {
		t.Fatalf("sufficientReputation: %v", err)
	}
	if !ok {
		t.Fatalf("expected sufficient at threshold")
	}

	// One msat below threshold => insufficient.
	ok, _, err = c.sufficientReputation(0, threshold-1, start)
	if err != nil {
		t.Fatalf("sufficientReputation: %v", err)
	}
	if ok {
		t.Fatalf("expected insufficient below threshold")
	}

	// At threshold but with in-flight risk => insufficient.
	ok, _, err = c.sufficientReputation(1, threshold, start)
	if err != nil {
		t.Fatalf("sufficientReputation: %v", err)
	}
	if ok {
		t.Fatalf("expected insufficient with in-flight risk")
	}
}

// isolationEvent builds a forward event for the white-box isolation tests.
func isolationEvent(start int64, height uint32) event {
	return event{
		incoming:      circuit(1, 0),
		outgoing:      circuit(2, 0),
		incomingAmt:   2000,
		outgoingAmt:   1000,
		advertisedFee: 1000,
		incomingCltv:  200,
		accountable:   true,
		at:            uint64(start),
		height:        height,
	}
}

// TestHookNeverBlocks asserts the forwarding hook never blocks the caller even
// when the worker is stalled and the queue is full: excess events are dropped
// (log-only) rather than back-pressuring the switch's forwarding goroutine.
func TestHookNeverBlocks(t *testing.T) {
	t.Parallel()

	// No worker is started, so nothing drains the queue.
	m := buildManagerNoStart(t, 1_000_000, 100)
	in, out := circuit(1, 0), circuit(2, 0)

	// Fill the queue to capacity so the next send would block if the hook
	// were blocking.
	for len(m.events) < cap(m.events) {
		m.events <- event{kind: evForward}
	}

	// The hook must return promptly (dropping the event), never block.
	done := make(chan struct{})
	go func() {
		m.OnForward(in, out, 2000, 1000, 1000, 200, false)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnForward blocked on a full queue")
	}
}

// TestDropOnOverflow asserts that an event dropped due to a full queue bumps
// the dropped counter.
func TestDropOnOverflow(t *testing.T) {
	t.Parallel()

	m := buildManagerNoStart(t, 1_000_000, 100)

	// Fill the queue so all further sends are dropped.
	for len(m.events) < cap(m.events) {
		m.events <- event{kind: evForward}
	}

	before := m.dropped.Load()
	m.OnForward(circuit(1, 0), circuit(2, 0), 2000, 1000, 1000, 200, false)
	m.OnSettle(circuit(1, 0), circuit(2, 0))

	if got := m.dropped.Load(); got != before+2 {
		t.Fatalf("dropped counter: got %d, want %d", got, before+2)
	}
}

// TestEventsProcessedAsync asserts a forward enqueued via the hook is applied
// by the worker (observed after sync).
func TestEventsProcessedAsync(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, _ := buildManager(t, start, 100)

	m.OnForward(circuit(1, 0), circuit(2, 0), 2000, 1000, 1000, 200, false)
	m.sync()

	if m.channels[2] == nil ||
		len(m.channels[2].pendingHTLCs) != 1 {

		t.Fatalf("worker did not process the forward event")
	}
}

// TestIsolationDecision drives the log-only isolation verdict through the
// white-box addHTLC path: zero reputation is insufficient, while ample
// reputation on the outgoing channel is sufficient. Uses a non-started manager
// so the worker-only methods can be called directly on the test goroutine.
func TestIsolationDecision(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	height := uint32(100)

	t.Run("zero reputation insufficient", func(t *testing.T) {
		m := buildManagerNoStart(t, start, height)

		// Give the incoming channel a positive revenue threshold so the
		// (zero) outgoing reputation is insufficient.
		inChan := m.getOrCreateChannel(1, uint64(start))
		if _, err := inChan.incomingRevenue.add(
			1_000_000, uint64(start),
		); err != nil {
			t.Fatalf("seed revenue: %v", err)
		}

		d, err := m.addHTLC(isolationEvent(start, height))
		if err != nil {
			t.Fatalf("addHTLC: %v", err)
		}
		if d.sufficient {
			t.Fatalf("expected insufficient, got %s", d)
		}
	})

	t.Run("ample reputation sufficient", func(t *testing.T) {
		m := buildManagerNoStart(t, start, height)

		// Give the outgoing channel ample reputation.
		outChan := m.getOrCreateChannel(2, uint64(start))
		if _, err := outChan.outgoingReputation.add(
			10_000_000, uint64(start),
		); err != nil {
			t.Fatalf("seed reputation: %v", err)
		}

		d, err := m.addHTLC(isolationEvent(start, height))
		if err != nil {
			t.Fatalf("addHTLC: %v", err)
		}
		if !d.sufficient {
			t.Fatalf("expected sufficient, got %s", d)
		}
	})
}
