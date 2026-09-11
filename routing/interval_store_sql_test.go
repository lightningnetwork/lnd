//go:build test_db_postgres || test_db_sqlite

package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestSQLIntervalStoreRoundTrip tests that a belief survives a trip through the
// database unchanged in everything the model reads.
func TestSQLIntervalStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := NewSQLIntervalStore(newIntervalTestDB(t))
	ctx := t.Context()

	written := []PersistedInterval{
		{
			Key: testIntervalKey,
			Interval: LiquidityInterval{
				LowerOK:    1000,
				UpperFail:  5000,
				Estimate:   2500,
				Confidence: 0.94,
				Successes:  3,
				Failures:   1,
				Mode:       intervalModeDepleted,
				Known:      true,
			},
		},
		{
			Key: testIntervalKey.Reverse(),
			Interval: LiquidityInterval{
				LowerOK:    7000,
				Estimate:   9000,
				Confidence: 0.5,
				Successes:  1,
				Mode:       intervalModeRich,
				Known:      true,
			},
		},
	}

	require.NoError(t, store.StoreIntervals(ctx, written))

	read, err := store.FetchIntervals(ctx, 100)
	require.NoError(t, err)
	require.Len(t, read, 2)

	byKey := make(map[IntervalKey]LiquidityInterval)
	for _, entry := range read {
		byKey[entry.Key] = entry.Interval
	}

	for _, entry := range written {
		got, ok := byKey[entry.Key]
		require.True(t, ok, "missing key %v", entry.Key)

		require.Equal(t, entry.Interval.LowerOK, got.LowerOK)
		require.Equal(t, entry.Interval.UpperFail, got.UpperFail)
		require.Equal(t, entry.Interval.Estimate, got.Estimate)
		require.Equal(t, entry.Interval.Successes, got.Successes)
		require.Equal(t, entry.Interval.Failures, got.Failures)
		require.Equal(t, entry.Interval.Mode, got.Mode)
		require.True(t, got.Known)

		// Confidence goes to disk as parts per million, so it comes
		// back to within one part in a million of itself.
		require.InDelta(
			t, entry.Interval.Confidence, got.Confidence, 1e-6,
		)

		// Nothing read from disk claims to be freshly observed. It is
		// the in-memory store that decides that when it seeds a belief
		// in, and it is what stops a restored bound from returning a
		// hard zero.
		require.False(t, got.Restored)
	}

	// Writing the same directed channel again replaces the belief rather
	// than adding a second row for it.
	replaced := written[0]
	replaced.Interval.UpperFail = 4000
	require.NoError(
		t, store.StoreIntervals(ctx, []PersistedInterval{replaced}),
	)

	read, err = store.FetchIntervals(ctx, 100)
	require.NoError(t, err)
	require.Len(t, read, 2)

	for _, entry := range read {
		if entry.Key == replaced.Key {
			require.EqualValues(t, 4000, entry.Interval.UpperFail)
		}
	}

	// Purging leaves nothing behind.
	require.NoError(t, store.PurgeIntervals(ctx))

	read, err = store.FetchIntervals(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, read)
}

// TestSQLIntervalStorePrune tests that the table can be held to a bound.
func TestSQLIntervalStorePrune(t *testing.T) {
	t.Parallel()

	store := NewSQLIntervalStore(newIntervalTestDB(t))
	ctx := t.Context()

	// Write in batches under a clock we step ourselves, so that the rows
	// carry distinct timestamps whatever resolution the backend stores
	// them at. Pruning keeps the most recently written.
	testClock := clock.NewTestClock(time.Unix(1_700_000_000, 0).UTC())
	store.clock = testClock

	const batches = 4
	for i := 0; i < batches; i++ {
		key := IntervalKey{
			ChanID: uint64(i),
			From:   route.Vertex{byte(i)},
			To:     route.Vertex{byte(i), 1},
		}

		require.NoError(t, store.StoreIntervals(
			ctx, []PersistedInterval{{
				Key: key,
				Interval: LiquidityInterval{
					Known:     true,
					UpperFail: lnwire.MilliSatoshi(i + 1),
				},
			}},
		))

		testClock.SetTime(testClock.Now().Add(time.Minute))
	}

	require.NoError(t, store.PruneIntervals(ctx, 2))

	read, err := store.FetchIntervals(ctx, 100)
	require.NoError(t, err)
	require.LessOrEqual(t, len(read), 2)
	require.NotEmpty(t, read)
}

// TestSQLIntervalStoreRestoresIntoMemory tests the whole path the router
// actually uses: beliefs written down by one store are read back by the next
// one to start, and arrive as soft evidence rather than as certainties.
func TestSQLIntervalStoreRestoresIntoMemory(t *testing.T) {
	t.Parallel()

	db := newIntervalTestDB(t)
	ctx := t.Context()
	capacity := testIntervalCapacity
	amt := capacity / 2

	// A node runs, learns that an amount does not fit, and shuts down.
	before := NewIntervalStore(0)
	before.UsePersistence(NewSQLIntervalStore(db), time.Millisecond)
	require.NoError(t, before.Start(ctx))

	before.RecordFailure(testIntervalKey, amt, capacity)
	require.Zero(t, before.Probability(testIntervalKey, amt, capacity))

	require.NoError(t, before.Stop())

	// A new node starts against the same database.
	after := NewIntervalStore(0)
	after.UsePersistence(NewSQLIntervalStore(db), time.Millisecond)
	require.NoError(t, after.Start(ctx))
	t.Cleanup(func() {
		require.NoError(t, after.Stop())
	})

	interval := after.Get(testIntervalKey, capacity)
	require.True(t, interval.Known)
	require.True(t, interval.Restored)
	require.Equal(t, amt, interval.UpperFail)

	// The bound came back, but it is no longer allowed to say impossible,
	// so the amount can be attempted again and the belief corrected.
	probability := after.Probability(testIntervalKey, amt, capacity)
	require.GreaterOrEqual(t, probability, intervalRestoredFloor)
	require.Less(t, probability, 0.5)

	// The reverse direction was written by the same observation and comes
	// back too.
	require.True(t, after.Get(testIntervalKey.Reverse(), capacity).Known)
}
