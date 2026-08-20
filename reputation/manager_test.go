package reputation

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// testHeight is the fixed best block height used by the manager tests. Forward
// events use an incoming cltv comfortably beyond it.
const testHeight = uint32(100)

// buildManager returns a started manager with an injected test clock at
// `start`. Per-channel state (incoming scid=1, outgoing scid=2) is created
// lazily on the first HTLC event.
func buildManager(t *testing.T, start int64) (*Manager, *clock.TestClock) {
	t.Helper()

	clk := clock.NewTestClock(time.Unix(start, 0))

	m, err := NewManager(DefaultConfig(), clk)
	require.NoError(t, err, "NewManager")
	require.NoError(t, m.Start(), "Start")
	t.Cleanup(func() { _ = m.Stop() })

	return m, clk
}

// TestManagerStartStop is a smoke test for the lifecycle.
func TestManagerStartStop(t *testing.T) {
	t.Parallel()

	m, err := NewManager(
		DefaultConfig(), clock.NewTestClock(time.Unix(1000, 0)),
	)
	require.NoError(t, err)
	require.NoError(t, m.Start())

	// Hooks on an empty manager must be safe no-ops (other than lazy
	// channel creation): they must never panic.
	m.OnSettle(circuit(1, 0), scid(2))
	m.OnFail(circuit(1, 0), scid(2))

	require.NoError(t, m.Stop())
}

// TestManagerRequiresClock checks that a manager cannot be built without a
// clock, since production and tests must both supply a real time source.
func TestManagerRequiresClock(t *testing.T) {
	t.Parallel()

	_, err := NewManager(DefaultConfig(), nil)
	require.Error(t, err)
}

// TestForwardSettleLifecycle exercises the pending lifecycle + reputation
// accrual: an unaccountable HTLC that settles quickly earns its fee. Because
// the hooks are synchronous, the effects are observable as soon as they return.
func TestForwardSettleLifecycle(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	m, clk := buildManager(t, start)

	in := circuit(1, 0)
	out := scid(2)

	// Required fee = 1000 (equals in-out here). cltv 200 > height 100.
	m.OnForward(in, out, 2000, 1000, 1000, 200, testHeight, false)

	outChan := m.channels[2]
	require.Len(t, outChan.pendingHTLCs, 1)

	// Settle 30s later (within resolution period).
	advance(clk, 30*time.Second)
	m.OnSettle(in, out)

	require.Empty(t, outChan.pendingHTLCs, "pending not cleared")

	rep, err := outChan.outgoingReputation.valueAt(clk.Now())
	require.NoError(t, err)
	require.EqualValues(t, 1000, rep, "reputation")

	// Incoming channel earned the fee as revenue.
	rev, err := m.channels[1].incomingRevenue.valueAt(clk.Now())
	require.NoError(t, err)
	require.Positive(t, rev, "revenue")
}

// TestFailDoesNotEarnRevenue checks that a failed unaccountable HTLC neither
// helps reputation nor adds revenue.
func TestFailDoesNotEarnRevenue(t *testing.T) {
	t.Parallel()

	m, clk := buildManager(t, 1_000_000)
	in, out := circuit(1, 0), scid(2)

	m.OnForward(in, out, 2000, 1000, 1000, 200, testHeight, false)
	advance(clk, 30*time.Second)
	m.OnFail(in, out)

	rep, err := m.channels[2].outgoingReputation.valueAt(clk.Now())
	require.NoError(t, err)
	require.EqualValues(t, 0, rep, "reputation after fail")

	rev, err := m.channels[1].incomingRevenue.valueAt(clk.Now())
	require.NoError(t, err)
	require.EqualValues(t, 0, rev, "revenue after fail")
}

// TestAccountableResolution drives accountable HTLCs through the full
// forward/resolve path. Unlike unaccountable HTLCs, accountable ones are
// charged the opportunity cost of the time they held the outgoing slot, so
// they are the only way a channel's reputation can decrease.
func TestAccountableResolution(t *testing.T) {
	t.Parallel()

	// The default resolution period is 90s, so resolving at 270s overruns
	// it by exactly 2x: opportunity cost = 2 * fee = 2000.
	const (
		fee  = 1000
		fast = 30 * time.Second
		slow = 270 * time.Second
	)

	tests := []struct {
		name    string
		hold    time.Duration
		settled bool
		wantRep int64
		wantRev int64
	}{{
		// Settling within the resolution period costs nothing, so the
		// HTLC earns its full fee just like an unaccountable one.
		name:    "settled fast earns fee",
		hold:    fast,
		settled: true,
		wantRep: fee,
		wantRev: fee,
	}, {
		// fee - 2*fee = -fee: holding the slot for too long costs more
		// than the forward earned.
		name:    "settled slow costs reputation",
		hold:    slow,
		settled: true,
		wantRep: -fee,
		wantRev: fee,
	}, {
		// A fast failure has no opportunity cost, but earns nothing
		// either.
		name:    "failed fast is neutral",
		hold:    fast,
		settled: false,
		wantRep: 0,
		wantRev: 0,
	}, {
		// A slow failure is pure cost: the fee was never earned, so
		// only the opportunity cost applies.
		name:    "failed slow costs reputation",
		hold:    slow,
		settled: false,
		wantRep: -2 * fee,
		wantRev: 0,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			m, clk := buildManager(t, 1_000_000)
			in, out := circuit(1, 0), scid(2)

			m.OnForward(
				in, out, 2000, 1000, fee, 200, testHeight, true,
			)

			advance(clk, test.hold)
			if test.settled {
				m.OnSettle(in, out)
			} else {
				m.OnFail(in, out)
			}

			outChan := m.channels[2]
			require.Empty(
				t, outChan.pendingHTLCs, "pending not cleared",
			)

			outRep := outChan.outgoingReputation
			rep, err := outRep.valueAt(clk.Now())
			require.NoError(t, err)
			require.Equal(t, test.wantRep, rep, "reputation")

			rev, err := m.channels[1].incomingRevenue.valueAt(
				clk.Now(),
			)
			require.NoError(t, err)
			require.Equal(t, test.wantRev, rev, "revenue")
		})
	}
}

// TestUnmatchedResolveNoop ensures a resolve with no matching forward is a safe
// no-op (tolerating a missed add / mid-flight enable).
func TestUnmatchedResolveNoop(t *testing.T) {
	t.Parallel()

	m, _ := buildManager(t, 1_000_000)

	// Should not panic or error fatally.
	m.OnSettle(circuit(1, 99), scid(2))
	m.OnFail(circuit(1, 88), scid(2))
}

// TestForwardRejectsExpiredCltv verifies OnForward refuses to track an HTLC
// whose incoming expiry is not beyond the current height (a condition that
// should never occur for a validly-accepted HTLC), leaving no pending state.
func TestForwardRejectsExpiredCltv(t *testing.T) {
	t.Parallel()

	m, _ := buildManager(t, 1_000_000)
	in, out := circuit(1, 0), scid(2)

	// incoming cltv == height: not beyond, must be rejected.
	m.OnForward(in, out, 2000, 1000, 1000, testHeight, testHeight, false)

	if c := m.channels[2]; c != nil {
		require.Empty(t, c.pendingHTLCs,
			"expired-cltv forward must not create a pending htlc")
	}
}

// TestStalePendingReported verifies that a pending HTLC which outlives its
// maximum hold time is reported, and deliberately NOT evicted: every resolution
// path removes its own pending, so a stale entry is a bug on our side that must
// stay visible rather than be quietly swept away.
func TestStalePendingReported(t *testing.T) {
	t.Parallel()

	m, clk := buildManager(t, 1_000_000)

	// cltv 101 at height 100 => max hold 600s.
	_, err := m.addHTLC(
		circuit(1, 0), scid(2), 2000, 1000, 1000, 101, testHeight,
		false, clk.Now(),
	)
	require.NoError(t, err, "addHTLC")

	// Before the maximum hold elapses nothing is stale.
	require.Zero(t, m.reportStalePendings())

	// Past the maximum hold it is reported, but left in place.
	advance(clk, 700*time.Second)
	require.Equal(t, 1, m.reportStalePendings())
	require.Len(t, m.channels[2].pendingHTLCs, 1,
		"stale pending must not be swept away")
}

// TestSufficiencyBoundary unit-tests the core reputation inequality at its
// boundary.
func TestSufficiencyBoundary(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	c := newChannelReputation(cfg, testStart)

	// Seed an incoming-revenue threshold of 1000 and read it back so the
	// aggregated average's warmup divisor is settled.
	_, err := c.incomingRevenue.add(1000, testStart)
	require.NoError(t, err)

	threshold, err := c.incomingRevenue.valueAt(testStart)
	require.NoError(t, err)

	// Reputation exactly at threshold, no in-flight risk => sufficient.
	noRisk := satFromInt(0)

	ok, _, err := c.sufficientReputation(noRisk, threshold, testStart)
	require.NoError(t, err)
	require.True(t, ok, "expected sufficient at threshold")

	// One msat below threshold => insufficient.
	ok, _, err = c.sufficientReputation(noRisk, threshold-1, testStart)
	require.NoError(t, err)
	require.False(t, ok, "expected insufficient below threshold")

	// At threshold but with in-flight risk => insufficient.
	ok, _, err = c.sufficientReputation(satFromInt(1), threshold, testStart)
	require.NoError(t, err)
	require.False(t, ok, "expected insufficient with in-flight risk")
}

// TestReputationDecision drives the log-only reputation verdict through the
// addHTLC path: zero reputation is insufficient, while ample reputation on the
// outgoing channel is sufficient.
func TestReputationDecision(t *testing.T) {
	t.Parallel()

	const start = 1_000_000
	at := time.Unix(start, 0)

	addHTLC := func(m *Manager) (decision, error) {
		return m.addHTLC(
			circuit(1, 0), scid(2), 2000, 1000, 1000, 200,
			testHeight, true, at,
		)
	}

	t.Run("zero reputation insufficient", func(t *testing.T) {
		t.Parallel()

		m, _ := buildManager(t, start)

		// Give the incoming channel a positive revenue threshold so the
		// (zero) outgoing reputation is insufficient.
		inChan := m.getOrCreateChannel(1, at)
		_, err := inChan.incomingRevenue.add(1_000_000, at)
		require.NoError(t, err, "seed revenue")

		d, err := addHTLC(m)
		require.NoError(t, err, "addHTLC")
		require.False(t, d.inIsolation, "expected insufficient: %s", d)
	})

	t.Run("ample reputation sufficient", func(t *testing.T) {
		t.Parallel()

		m, _ := buildManager(t, start)

		// Give the outgoing channel ample reputation.
		outChan := m.getOrCreateChannel(2, at)
		_, err := outChan.outgoingReputation.add(10_000_000, at)
		require.NoError(t, err, "seed reputation")

		d, err := addHTLC(m)
		require.NoError(t, err, "addHTLC")
		require.True(t, d.inIsolation, "expected sufficient: %s", d)
	})
}
