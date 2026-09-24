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
func newTestSQLStore(t *testing.T) (*SQLStore, BatchedSQLQueries) {
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
// timestamps derived from the scid so round trips can be compared exactly.
func testChannelState(scid uint64) ChannelState {
	base := time.Unix(1_700_000_000+int64(scid), 0).UTC()

	return ChannelState{
		SCID:                        scid,
		OutgoingReputation:          int64(scid) * 1_000,
		OutgoingReputationUpdatedAt: base,
		IncomingRevenue:             int64(scid) * 10,
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

	require.NoError(t, store.DeleteChannel(ctx, 42))

	// Upserting nothing is a no-op.
	require.NoError(t, store.UpsertChannels(ctx, nil))
}

// TestSQLStoreRoundTrip checks that channel states survive a write and read
// unchanged, including negative reputation, and come back ordered by scid.
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

	// Rows come back ordered by scid.
	require.Equal(t, []ChannelState{want[2], want[0], want[1]}, got)
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

	require.NoError(t, store.DeleteChannel(ctx, drop.SCID))

	got, err := store.FetchChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelState{keep}, got)

	// Deleting it again is not an error.
	require.NoError(t, store.DeleteChannel(ctx, drop.SCID))
}

// TestSQLStoreMaxSCID checks that a short channel id using the full uint64
// range survives the big endian encoding.
func TestSQLStoreMaxSCID(t *testing.T) {
	t.Parallel()

	store, _ := newTestSQLStore(t)
	ctx := t.Context()

	state := testChannelState(1)
	state.SCID = ^uint64(0)
	require.NoError(t, store.UpsertChannels(ctx, []ChannelState{state}))

	got, err := store.FetchChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelState{state}, got)
}

// TestSQLStoreCorruptSCID checks that a row whose scid is not 8 bytes is
// reported as an error instead of being decoded into a bogus channel.
func TestSQLStoreCorruptSCID(t *testing.T) {
	t.Parallel()

	store, db := newTestSQLStore(t)
	ctx := t.Context()

	// Insert a malformed row directly, bypassing the store's encoding.
	err := db.ExecTx(ctx, sqldb.WriteTxOpt(), func(q SQLQueries) error {
		return q.UpsertReputationChannel(
			ctx, sqlc.UpsertReputationChannelParams{
				Scid:                        []byte{1, 2, 3},
				OutgoingReputationUpdatedAt: time.Now().UTC(),
				IncomingRevenueUpdatedAt:    time.Now().UTC(),
				IncomingRevenueStartedAt:    time.Now().UTC(),
			},
		)
	}, sqldb.NoOpReset)
	require.NoError(t, err)

	_, err = store.FetchChannels(ctx)
	require.ErrorContains(t, err, "invalid scid length 3")
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

	require.NoError(t, store.DeleteChannel(ctx, 1))
}
