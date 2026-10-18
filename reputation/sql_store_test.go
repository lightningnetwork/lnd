package reputation

import (
	"database/sql"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// newTestSQLStore creates an SQLStore on a fresh test database, returning the
// underlying queries as well so tests can bypass the store.
func newTestSQLStore(t testing.TB) (*SQLStore, BatchedSQLQueries) {
	t.Helper()

	db := newTestDB(t)
	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)

	return NewSQLStore(executor), executor
}

// testChannelState builds a channel state with distinct, second precision
// timestamps derived from n so round trips can be compared exactly. The
// channel_id is chanID(n), which orders rows by n.
func testChannelState(n uint64) ChannelState {
	base := time.Unix(1_700_000_000+int64(n), 0).UTC()

	return ChannelState{
		ChanID:                      chanID(n),
		OutgoingReputation:          int64(n) * 1_000,
		OutgoingReputationUpdatedAt: base,
		IncomingRevenue:             int64(n) * 10,
		IncomingRevenueUpdatedAt:    base.Add(time.Minute),
		IncomingRevenueStartedAt:    base.Add(-time.Hour),
	}
}

// TestSQLStoreEmpty checks that a fresh store holds no channels and that
// deleting an unknown channel is not an error.
func TestSQLStoreEmpty(t *testing.T) {
	t.Parallel()

	store, _ := newTestSQLStore(t)
	ctx := t.Context()

	channels, err := store.FetchChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, channels)

	require.NoError(t, store.DeleteChannel(ctx, chanID(42)))

	// Upserting nothing is a no-op.
	require.NoError(t, store.UpsertChannels(ctx, nil))
}

// TestSQLStoreRoundTrip checks that channel states survive a write and read
// unchanged, including negative reputation, and come back ordered by
// channel_id.
func TestSQLStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store, _ := newTestSQLStore(t)
	ctx := t.Context()

	negative := testChannelState(7)
	negative.OutgoingReputation = -12_345

	want := []ChannelState{
		testChannelState(3), negative, testChannelState(1),
	}
	require.NoError(t, store.UpsertChannels(ctx, want))

	got, err := store.FetchChannels(ctx)
	require.NoError(t, err)

	// Rows come back ordered by channel_id.
	require.Equal(t, []ChannelState{want[2], want[0], want[1]}, got)
}

// TestSQLStoreTimestampPrecision checks how precisely timestamps survive the
// round trip. The manager stamps state with the wall clock, so nanosecond
// timestamps must come back within a microsecond, which is what the backends
// store, and never shifted by more than that.
func TestSQLStoreTimestampPrecision(t *testing.T) {
	t.Parallel()

	store, _ := newTestSQLStore(t)
	ctx := t.Context()

	want := testChannelState(1)
	ts := time.Unix(1_700_000_000, 123_456_789).UTC()
	want.OutgoingReputationUpdatedAt = ts
	want.IncomingRevenueUpdatedAt = ts.Add(time.Nanosecond)
	want.IncomingRevenueStartedAt = ts.Add(-time.Nanosecond)
	require.NoError(t, store.UpsertChannels(ctx, []ChannelState{want}))

	got, err := store.FetchChannels(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)

	require.WithinDuration(
		t, want.OutgoingReputationUpdatedAt,
		got[0].OutgoingReputationUpdatedAt, time.Microsecond,
	)
	require.WithinDuration(
		t, want.IncomingRevenueUpdatedAt,
		got[0].IncomingRevenueUpdatedAt, time.Microsecond,
	)
	require.WithinDuration(
		t, want.IncomingRevenueStartedAt,
		got[0].IncomingRevenueStartedAt, time.Microsecond,
	)
}

// TestSQLStoreUpsertReplaces checks that writing a channel again replaces its
// previous state rather than adding a second row.
func TestSQLStoreUpsertReplaces(t *testing.T) {
	t.Parallel()

	store, _ := newTestSQLStore(t)
	ctx := t.Context()

	first := testChannelState(5)
	require.NoError(t, store.UpsertChannels(ctx, []ChannelState{first}))

	second := first
	second.OutgoingReputation = 999
	second.OutgoingReputationUpdatedAt = first.OutgoingReputationUpdatedAt.
		Add(time.Hour)
	second.IncomingRevenue = 1
	require.NoError(t, store.UpsertChannels(ctx, []ChannelState{second}))

	got, err := store.FetchChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelState{second}, got)
}

// TestSQLStoreDelete checks that deleting a channel removes only that channel.
func TestSQLStoreDelete(t *testing.T) {
	t.Parallel()

	store, _ := newTestSQLStore(t)
	ctx := t.Context()

	keep, drop := testChannelState(1), testChannelState(2)
	require.NoError(t, store.UpsertChannels(
		ctx, []ChannelState{keep, drop},
	))

	require.NoError(t, store.DeleteChannel(ctx, drop.ChanID))

	got, err := store.FetchChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelState{keep}, got)

	// Deleting it again is not an error.
	require.NoError(t, store.DeleteChannel(ctx, drop.ChanID))
}

// TestSQLStoreAllBitsChannelID checks that a channel_id with every bit set
// survives the round trip unchanged.
func TestSQLStoreAllBitsChannelID(t *testing.T) {
	t.Parallel()

	store, _ := newTestSQLStore(t)
	ctx := t.Context()

	state := testChannelState(1)
	for i := range state.ChanID {
		state.ChanID[i] = 0xff
	}
	require.NoError(t, store.UpsertChannels(ctx, []ChannelState{state}))

	got, err := store.FetchChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelState{state}, got)
}

// TestSQLStoreCorruptChannelID checks that a row whose channel_id is not 32
// bytes is reported as an error instead of being decoded into a bogus channel.
func TestSQLStoreCorruptChannelID(t *testing.T) {
	t.Parallel()

	store, db := newTestSQLStore(t)
	ctx := t.Context()

	// Insert a malformed row directly, bypassing the store's encoding.
	err := db.ExecTx(ctx, sqldb.WriteTxOpt(), func(q SQLQueries) error {
		return q.UpsertReputationChannel(
			ctx, sqlc.UpsertReputationChannelParams{
				ChannelID:                   []byte{1, 2, 3},
				OutgoingReputationUpdatedAt: time.Now().UTC(),
				IncomingRevenueUpdatedAt:    time.Now().UTC(),
				IncomingRevenueStartedAt:    time.Now().UTC(),
			},
		)
	}, sqldb.NoOpReset)
	require.NoError(t, err)

	_, err = store.FetchChannels(ctx)
	require.ErrorContains(t, err, "invalid channel_id length 3")
}

// TestNoopStore checks that the no-op store holds nothing and never errors.
func TestNoopStore(t *testing.T) {
	t.Parallel()

	store := NewNoopStore()
	ctx := t.Context()

	require.NoError(t, store.UpsertChannels(
		ctx, []ChannelState{testChannelState(1)},
	))

	channels, err := store.FetchChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, channels)

	require.NoError(t, store.DeleteChannel(ctx, chanID(1)))
}
