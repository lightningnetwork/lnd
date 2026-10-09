package reputation

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestReplayInFlight checks that replayed HTLCs are tracked exactly like live
// forwards: they are pending on their outgoing channel, indexed by circuit
// key, stamped with the replay time, and only accountable ones add risk.
func TestReplayInFlight(t *testing.T) {
	t.Parallel()

	clk := clock.NewTestClock(time.Unix(1_000_000, 0))
	m := startManager(t, shortWindowConfig(), clk, nil)

	// Advance so the replay time differs from the manager start time.
	advance(clk, time.Hour)
	now := clk.Now()

	accountable := InFlightHTLC{
		Incoming: circuit(1, 0), Outgoing: scid(2), Fee: 1000,
		IncomingCltv: testHeight + 1, Accountable: true,
	}
	unaccountable := InFlightHTLC{
		Incoming: circuit(1, 1), Outgoing: scid(2), Fee: 1000,
		IncomingCltv: testHeight + 1, Accountable: false,
	}
	otherChan := InFlightHTLC{
		Incoming: circuit(3, 0), Outgoing: scid(4), Fee: 500,
		IncomingCltv: testHeight + 10, Accountable: true,
	}

	n := m.ReplayInFlight(
		[]InFlightHTLC{accountable, unaccountable, otherChan},
		testHeight,
	)
	require.Equal(t, 3, n)

	require.Len(t, m.channels[2].pendingHTLCs, 2)
	require.Len(t, m.channels[4].pendingHTLCs, 1)
	require.Len(t, m.htlcIndex, 3)
	require.EqualValues(t, 2, m.htlcIndex[accountable.Incoming])
	require.EqualValues(t, 4, m.htlcIndex[otherChan.Incoming])

	// Stamped with the replay time, and the hold measured from the replay
	// height: cltv delta 1 gives a 600s worst case hold.
	p := m.channels[2].pendingHTLCs[accountable.Incoming]
	require.Equal(t, now, p.addedAt)
	require.Equal(t, 600*time.Second, p.maxHold)
	require.True(t, p.accountable)
	require.EqualValues(t, 1000, p.fee)

	// Only the accountable HTLC contributes risk: round((600-90)/90 *
	// 1000) = 5667.
	require.EqualValues(t, 5667, m.channels[2].inFlightRisk().Int64())

	// A replayed HTLC resolves like any other. Settling the accountable one
	// 30s after the replay earns its full fee, since the hold is measured
	// from the replay, not from some unknown original forward time.
	advance(clk, 30*time.Second)
	m.OnSettle(accountable.Incoming)

	require.NotContains(t, m.htlcIndex, accountable.Incoming)
	require.Len(t, m.channels[2].pendingHTLCs, 1)

	rep := m.channels[2].outgoingReputation.valueAt(clk.Now())
	require.EqualValues(t, 1000, rep)

	rev := m.channels[1].incomingRevenue.valueAt(clk.Now())
	require.EqualValues(t, 1000, rev)

	// A replayed HTLC that fails is scored too. The unaccountable one
	// contributes nothing on failure and is cleared.
	m.OnFail(unaccountable.Incoming)
	require.NotContains(t, m.htlcIndex, unaccountable.Incoming)
	require.Empty(t, m.channels[2].pendingHTLCs)

	rep = m.channels[2].outgoingReputation.valueAt(clk.Now())
	require.EqualValues(t, 1000, rep, "unaccountable fail changed rep")

	// The accountable one on channel 4 fails after being held past the
	// resolution period since the replay, so it is charged the
	// opportunity cost of the hold measured from the replay: 5 minutes
	// held on a 90s period with a 500 msat fee is round((300-90)/90 *
	// 500) = 1167 docked.
	advance(clk, 270*time.Second)
	m.OnFail(otherChan.Incoming)
	require.Empty(t, m.htlcIndex)

	rep = m.channels[4].outgoingReputation.valueAt(clk.Now())
	require.EqualValues(t, -1167, rep, "accountable fail not charged")
}

// TestReplayInFlightSkipsUntrackable checks that HTLCs which cannot be tracked
// are skipped without disturbing the rest: an expired incoming cltv, and a
// circuit that is already pending.
func TestReplayInFlightSkipsUntrackable(t *testing.T) {
	t.Parallel()

	clk := clock.NewTestClock(time.Unix(1_000_000, 0))
	m := startManager(t, shortWindowConfig(), clk, nil)

	// Already known from live traffic.
	live := circuit(1, 0)
	m.OnForward(live, scid(2), 2000, 1000, 1000, 200, testHeight, true)

	n := m.ReplayInFlight([]InFlightHTLC{
		{
			// Duplicate of the live forward, on a different channel
			// even: must not move the index.
			Incoming: live, Outgoing: scid(3), Fee: 1000,
			IncomingCltv: 200, Accountable: true,
		},
		{
			// Expired: cltv not beyond the current height.
			Incoming: circuit(1, 1), Outgoing: scid(2), Fee: 1000,
			IncomingCltv: testHeight, Accountable: true,
		},
		{
			// Fine.
			Incoming: circuit(1, 2), Outgoing: scid(2), Fee: 1000,
			IncomingCltv: 200, Accountable: true,
		},
	}, testHeight)
	require.Equal(t, 1, n)

	require.Len(t, m.htlcIndex, 2)
	require.EqualValues(t, 2, m.htlcIndex[live], "index moved")
	require.Contains(t, m.htlcIndex, circuit(1, 2))
	require.NotContains(t, m.htlcIndex, circuit(1, 1))
	require.Len(t, m.channels[2].pendingHTLCs, 2)

	// Replaying nothing is fine.
	require.Zero(t, m.ReplayInFlight(nil, testHeight))
}

// TestReplayOntoRestoredChannel checks that replayed HTLCs attach to channel
// state restored from the store rather than resetting it, so a restart keeps
// both the reputation and the in-flight risk.
func TestReplayOntoRestoredChannel(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_000_000, 0)
	store := newMemStore()
	store.channels[2] = ChannelState{
		SCID:                        2,
		OutgoingReputation:          50_000,
		OutgoingReputationUpdatedAt: now,
		IncomingRevenueUpdatedAt:    now,
		IncomingRevenueStartedAt:    now,
	}

	m := startManager(
		t, shortWindowConfig(), clock.NewTestClock(now), store,
	)

	n := m.ReplayInFlight([]InFlightHTLC{{
		Incoming: circuit(1, 0), Outgoing: scid(2), Fee: 1000,
		IncomingCltv: testHeight + 1, Accountable: true,
	}}, testHeight)
	require.Equal(t, 1, n)

	c := m.channels[2]
	require.Len(t, c.pendingHTLCs, 1)

	rep := c.outgoingReputation.valueAt(now)
	require.EqualValues(t, 50_000, rep, "restored reputation lost")
	require.EqualValues(t, 5667, c.inFlightRisk().Int64())
}
