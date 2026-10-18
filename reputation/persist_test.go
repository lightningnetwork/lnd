package reputation

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// memStore is an in-memory Store with injectable failures, used to drive the
// manager's persistence paths without a database.
type memStore struct {
	mu       sync.Mutex
	channels map[lnwire.ChannelID]ChannelState

	upserts int
	deletes int

	failFetch  error
	failUpsert error
	failDelete error

	// upsertStarted and upsertRelease, when set, make UpsertChannels
	// signal that it has started and then wait to be released, so a test
	// can interleave other calls with an in-progress write.
	upsertStarted chan struct{}
	upsertRelease chan struct{}
}

func newMemStore() *memStore {
	return &memStore{channels: make(map[lnwire.ChannelID]ChannelState)}
}

func (s *memStore) FetchChannels(context.Context) ([]ChannelState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failFetch != nil {
		return nil, s.failFetch
	}

	states := make([]ChannelState, 0, len(s.channels))
	for _, c := range s.channels {
		states = append(states, c)
	}

	return states, nil
}

func (s *memStore) UpsertChannels(_ context.Context,
	channels []ChannelState) error {

	if s.upsertStarted != nil {
		s.upsertStarted <- struct{}{}
		<-s.upsertRelease
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failUpsert != nil {
		return s.failUpsert
	}

	s.upserts++
	for _, c := range channels {
		s.channels[c.ChanID] = c
	}

	return nil
}

func (s *memStore) DeleteChannel(_ context.Context,
	chanID lnwire.ChannelID) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failDelete != nil {
		return s.failDelete
	}

	s.deletes++
	delete(s.channels, chanID)

	return nil
}

func (s *memStore) get(t *testing.T, chanID lnwire.ChannelID) ChannelState {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.channels[chanID]
	require.True(t, ok, "channel %v not in store", chanID)

	return c
}

func (s *memStore) has(chanID lnwire.ChannelID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.channels[chanID]

	return ok
}

// shortWindowConfig returns a config with windows short enough for the decay
// to be visible within a few minutes of test clock time: a 100s reputation
// window and a 600s (6 x 100s) revenue window.
func shortWindowConfig() Config {
	return Config{
		ResolutionPeriod:     90 * time.Second,
		RevenueWindow:        100 * time.Second,
		ReputationMultiplier: 1,
		RevenueWindowCount:   6,
	}
}

// startManager builds and starts a manager on the given clock and store.
func startManager(t *testing.T, cfg Config, clk clock.Clock,
	store Store) *Manager {

	t.Helper()

	m, err := NewManager(cfg, clk, store)
	require.NoError(t, err, "NewManager")
	require.NoError(t, m.Start(), "Start")
	t.Cleanup(func() { _ = m.Stop() })

	return m
}

// settleForward drives one forward and settle of a 1000 msat fee HTLC from
// channel 1 to channel 2 through the manager, resolving hold seconds later.
func settleForward(t *testing.T, m *Manager, clk *clock.TestClock,
	htlcID uint64, hold time.Duration) {

	t.Helper()

	in := circuit(1, htlcID)
	m.OnForward(
		in, chanID(1), chanID(2), 2000, 1000, 1000, 200, testHeight,
		false,
	)
	require.Contains(t, m.htlcIndex, in, "forward not recorded")

	advance(clk, hold)
	m.OnSettle(in)
	require.NotContains(t, m.htlcIndex, in, "settle not applied")
}

// averages reads both averages of the manager's channels 1 and 2 at now.
func averages(t *testing.T, m *Manager, now time.Time) (int64, int64) {
	t.Helper()

	rep := m.channels[chanID(2)].outgoingReputation.valueAt(now)

	rev := m.channels[chanID(1)].incomingRevenue.valueAt(now)

	return rep, rev
}

// testRestartIsTransparent checks that a manager restarted from the store after
// a period of downtime reads exactly the same averages as one that never
// stopped: the persisted timestamps are restored verbatim, so the downtime
// decays the values instead of being skipped, and the warm-up factor keeps
// advancing from the original start.
func testRestartIsTransparent(t *testing.T, store Store) {
	t.Helper()

	cfg := shortWindowConfig()
	start := time.Unix(1_000_000, 0)

	// The reference manager keeps running across the whole test.
	refClk := clock.NewTestClock(start)
	ref := startManager(t, cfg, refClk, nil)
	settleForward(t, ref, refClk, 0, 30*time.Second)

	// The persisted manager does the same forward, then stops, which
	// flushes its state to the store.
	clk := clock.NewTestClock(start)
	m1 := startManager(t, cfg, clk, store)
	settleForward(t, m1, clk, 0, 30*time.Second)
	require.NoError(t, m1.Stop(), "Stop")

	// Both channels were written: 2 earned reputation, 1 earned revenue.
	states, err := store.FetchChannels(t.Context())
	require.NoError(t, err)
	require.Len(t, states, 2)

	// A full reputation window of downtime, then a restart from the store.
	const downtime = 100 * time.Second
	advance(clk, downtime)
	advance(refClk, downtime)

	m2 := startManager(t, cfg, clk, store)
	require.Len(t, m2.channels, 2, "channels not restored")

	// The timestamps must be the persisted ones, not the load time. At
	// 1000 * e^(-1) = 368 the reputation is visibly decayed.
	settledAt := start.Add(30 * time.Second)
	restoredRep := m2.channels[chanID(2)].outgoingReputation
	require.WithinDuration(t, settledAt, restoredRep.lastUpdated, 0,
		"reputation timestamp re-stamped on load")
	restoredRev := m2.channels[chanID(1)].incomingRevenue
	require.WithinDuration(t, start, restoredRev.start, 0,
		"revenue start re-stamped on load")

	wantRep, wantRev := averages(t, ref, refClk.Now())
	gotRep, gotRev := averages(t, m2, clk.Now())
	require.EqualValues(t, 368, gotRep, "reputation after downtime")
	require.Equal(t, wantRep, gotRep, "reputation differs from reference")
	require.Equal(t, wantRev, gotRev, "revenue differs from reference")

	// Forwarding continues on top of the restored state, in step with the
	// reference: 368 decayed further, plus the new fee.
	settleForward(t, ref, refClk, 1, 30*time.Second)
	settleForward(t, m2, clk, 1, 30*time.Second)

	wantRep, wantRev = averages(t, ref, refClk.Now())
	gotRep, gotRev = averages(t, m2, clk.Now())
	require.Equal(t, wantRep, gotRep, "reputation after restart")
	require.Equal(t, wantRev, gotRev, "revenue after restart")
	require.Greater(t, gotRep, int64(1000), "new fee not added on top")
}

// TestRestartIsTransparent runs the restart check against the in-memory store.
func TestRestartIsTransparent(t *testing.T) {
	t.Parallel()

	testRestartIsTransparent(t, newMemStore())
}

// TestRestartIsTransparentSQL runs the restart check against the SQL store, so
// the timestamp round trip through the database is covered too.
func TestRestartIsTransparentSQL(t *testing.T) {
	t.Parallel()

	db := newTestDB(t)
	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)

	testRestartIsTransparent(t, NewSQLStore(executor))
}

// TestLoadClampsFutureTimestamps checks that persisted timestamps lying in the
// future, which happens when the clock went backwards across a restart, are
// clamped to now on load so the state stays usable.
func TestLoadClampsFutureTimestamps(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_000_000, 0)
	future := now.Add(time.Hour)

	store := newMemStore()
	store.channels[chanID(2)] = ChannelState{
		ChanID:                      chanID(2),
		OutgoingReputation:          5000,
		OutgoingReputationUpdatedAt: future,
		IncomingRevenue:             700,
		IncomingRevenueUpdatedAt:    future,
		IncomingRevenueStartedAt:    future,
	}

	m := startManager(
		t, shortWindowConfig(), clock.NewTestClock(now), store,
	)
	c, ok := m.channels[chanID(2)]
	require.True(t, ok, "channel not loaded")

	// All three timestamps were clamped, so no decay is applied and reads
	// at now succeed instead of failing as backwards time.
	require.Equal(t, now, c.outgoingReputation.lastUpdated)
	require.Equal(t, now, c.incomingRevenue.inner.lastUpdated)
	require.Equal(t, now, c.incomingRevenue.start)

	rep := c.outgoingReputation.valueAt(now)
	require.EqualValues(t, 5000, rep)

	rev := c.incomingRevenue.valueAt(now)
	require.EqualValues(t, 700, rev)
}

// TestLoadKeepsDecayedChannels checks that a channel whose reputation and
// revenue have both decayed to zero is still restored, with its revenue start
// time intact, since that start time keeps driving the warm-up factor.
func TestLoadKeepsDecayedChannels(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_000_000, 0)
	cfg := shortWindowConfig()

	store := newMemStore()

	// Channel 2 last earned a tiny amount a hundred windows ago.
	longAgo := now.Add(-100 * cfg.reputationWindow())
	store.channels[chanID(2)] = ChannelState{
		ChanID:                      chanID(2),
		OutgoingReputation:          10,
		OutgoingReputationUpdatedAt: longAgo,
		IncomingRevenue:             10,
		IncomingRevenueUpdatedAt:    longAgo,
		IncomingRevenueStartedAt:    longAgo,
	}

	m := startManager(t, cfg, clock.NewTestClock(now), store)

	c, ok := m.channels[chanID(2)]
	require.True(t, ok, "decayed channel not loaded")
	require.Zero(t, c.outgoingReputation.valueAt(now))
	require.Zero(t, c.incomingRevenue.valueAt(now))
	require.Equal(t, longAgo, c.incomingRevenue.start, "start re-stamped")

	require.True(t, store.has(chanID(2)), "row dropped from store")
	require.Zero(t, store.deletes)
}

// TestFlushDoesNotResurrectRemovedChannel checks that a flush whose snapshot
// was taken before a channel was removed cannot write that channel back to the
// store after the removal: the two are serialised, so the removal waits for
// the write and then deletes the row.
func TestFlushDoesNotResurrectRemovedChannel(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.upsertStarted = make(chan struct{})
	store.upsertRelease = make(chan struct{})

	clk := clock.NewTestClock(time.Unix(1_000_000, 0))
	m := startManager(t, shortWindowConfig(), clk, store)
	settleForward(t, m, clk, 0, 30*time.Second)

	// Start a flush and hold it inside the store write, after it took its
	// snapshot of channel 2.
	flushDone := make(chan error, 1)
	go func() {
		flushDone <- m.flush()
	}()
	<-store.upsertStarted

	// Remove channel 2 while the write is in progress.
	removeDone := make(chan error, 1)
	go func() {
		removeDone <- m.RemoveChannel(chanID(2))
	}()

	// The removal must not complete before the write does.
	select {
	case err := <-removeDone:
		t.Fatalf("removal ran during flush: %v", err)

	case <-time.After(50 * time.Millisecond):
	}

	store.upsertRelease <- struct{}{}
	require.NoError(t, <-flushDone)
	require.NoError(t, <-removeDone)

	require.False(t, store.has(chanID(2)), "removed channel resurrected")
	require.True(t, store.has(chanID(1)), "unrelated channel lost")
}

// TestStartFailsWhenStoreUnreadable checks that a store that cannot be read
// fails Start rather than silently starting from empty state.
func TestStartFailsWhenStoreUnreadable(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.failFetch = errTest

	m, err := NewManager(
		shortWindowConfig(), clock.NewTestClock(time.Unix(1, 0)), store,
	)
	require.NoError(t, err)
	require.ErrorIs(t, m.Start(), errTest)
}

// TestFlushWritesOnlyChangedChannels checks that a flush writes exactly the
// channels whose averages changed since the previous flush.
func TestFlushWritesOnlyChangedChannels(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	clk := clock.NewTestClock(time.Unix(1_000_000, 0))
	m := startManager(t, shortWindowConfig(), clk, store)

	// Nothing has changed yet: no write.
	require.NoError(t, m.flush())
	require.Equal(t, 0, store.upserts)

	// A settle changes both the outgoing channel's reputation and the
	// incoming channel's revenue.
	settleForward(t, m, clk, 0, 30*time.Second)
	require.Len(t, m.dirty, 2)

	require.NoError(t, m.flush())
	require.Equal(t, 1, store.upserts)
	require.Empty(t, m.dirty, "dirty set not cleared")
	require.EqualValues(t, 1000, store.get(t, chanID(2)).OutgoingReputation)
	require.EqualValues(t, 1000, store.get(t, chanID(1)).IncomingRevenue)
	require.Equal(
		t, clk.Now(),
		store.get(t, chanID(2)).OutgoingReputationUpdatedAt,
	)

	// Nothing changed since: no write.
	require.NoError(t, m.flush())
	require.Equal(t, 1, store.upserts)

	// A failed HTLC only touches the outgoing channel.
	in := circuit(1, 1)
	m.OnForward(
		in, chanID(1), chanID(2), 2000, 1000, 1000, 200, testHeight,
		false,
	)
	m.OnFail(in)
	require.Len(t, m.dirty, 1)
	require.Contains(t, m.dirty, chanID(2))
}

// TestFlushRetriesAfterStoreError checks that a failed write keeps the channels
// marked so the next flush retries them, and that Stop reports the failure.
func TestFlushRetriesAfterStoreError(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	clk := clock.NewTestClock(time.Unix(1_000_000, 0))
	m := startManager(t, shortWindowConfig(), clk, store)

	settleForward(t, m, clk, 0, 30*time.Second)

	store.failUpsert = errTest
	require.ErrorIs(t, m.flush(), errTest)
	require.Len(t, m.dirty, 2, "channels dropped from dirty set on error")
	require.False(t, store.has(chanID(2)))

	store.failUpsert = nil
	require.NoError(t, m.flush())
	require.Empty(t, m.dirty)
	require.True(t, store.has(chanID(2)))
	require.True(t, store.has(chanID(1)))

	// Stop surfaces a failing final flush.
	settleForward(t, m, clk, 1, 30*time.Second)
	store.failUpsert = errTest
	require.ErrorIs(t, m.Stop(), errTest)
}

// TestRemoveChannel checks that removing a channel drops it from memory and the
// store, forgets the HTLCs pending on it, and leaves other channels alone.
func TestRemoveChannel(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	clk := clock.NewTestClock(time.Unix(1_000_000, 0))
	m := startManager(t, shortWindowConfig(), clk, store)

	// Channel 2 has reputation in the store and an HTLC pending on it.
	settleForward(t, m, clk, 0, 30*time.Second)
	require.NoError(t, m.flush())

	pending := circuit(1, 1)
	m.OnForward(
		pending, chanID(1), chanID(2), 2000, 1000, 1000, 200,
		testHeight, true,
	)
	require.Contains(t, m.htlcIndex, pending)

	// Channel 2 is also the incoming channel of an HTLC pending on
	// channel 4, which must be forgotten too: settling it later would
	// otherwise recreate channel 2 to credit its fee.
	arrived := circuit(2, 0)
	m.OnForward(
		arrived, chanID(2), chanID(4), 2000, 1000, 1000, 200,
		testHeight, true,
	)

	// Channel 3 is unrelated and must be untouched.
	other := circuit(3, 0)
	m.OnForward(
		other, chanID(3), chanID(4), 2000, 1000, 1000, 200, testHeight,
		true,
	)

	require.NoError(t, m.RemoveChannel(chanID(2)))

	require.NotContains(t, m.channels, chanID(2))
	require.NotContains(t, m.htlcIndex, pending, "pending not forgotten")
	require.NotContains(t, m.htlcIndex, arrived, "arrived not forgotten")
	require.NotContains(t, m.dirty, chanID(2))
	require.False(t, store.has(chanID(2)), "store row not deleted")

	require.Contains(t, m.channels, chanID(4))
	require.Len(t, m.channels[chanID(4)].pendingHTLCs, 1)
	require.Contains(t, m.htlcIndex, other)
	require.True(t, store.has(chanID(1)))

	// A late resolution for either forgotten HTLC is a harmless no-op.
	m.OnSettle(pending)
	m.OnSettle(arrived)
	require.NotContains(t, m.channels, chanID(2), "channel recreated")

	// Removing a channel we know nothing about is fine, and a failing
	// store delete is reported.
	require.NoError(t, m.RemoveChannel(chanID(99)))

	store.failDelete = errTest
	require.ErrorIs(t, m.RemoveChannel(chanID(4)), errTest)
}

// TestNoopStoreManager checks the manager works unchanged without a store: a
// restart starts from empty state.
func TestNoopStoreManager(t *testing.T) {
	t.Parallel()

	clk := clock.NewTestClock(time.Unix(1_000_000, 0))
	m1 := startManager(t, shortWindowConfig(), clk, nil)
	settleForward(t, m1, clk, 0, 30*time.Second)
	require.NoError(t, m1.Stop())

	m2 := startManager(t, shortWindowConfig(), clk, nil)
	require.Empty(t, m2.channels)
}
