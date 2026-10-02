package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	// testIntervalCapacity is the capacity used by the interval tests. It
	// is large enough that the fractional thresholds of the model land on
	// distinct amounts.
	testIntervalCapacity = lnwire.MilliSatoshi(1_000_000_000)

	// testIntervalKey is the directed channel the tests observe.
	testIntervalKey = IntervalKey{
		ChanID: 7,
		From:   route.Vertex{1},
		To:     route.Vertex{2},
	}
)

// TestIntervalNormalize tests that the interval invariants hold no matter what
// combination of bounds is written into the structure.
func TestIntervalNormalize(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity

	tests := []struct {
		name     string
		interval LiquidityInterval
		check    func(*testing.T, LiquidityInterval)
	}{
		{
			name: "upper bound contradicting lower bound is " +
				"dropped",
			interval: LiquidityInterval{
				LowerOK:   500,
				UpperFail: 400,
			},
			check: func(t *testing.T, l LiquidityInterval) {
				require.EqualValues(t, 500, l.LowerOK)
				require.EqualValues(t, 0, l.UpperFail)
			},
		},
		{
			name: "bounds above capacity are clamped",
			interval: LiquidityInterval{
				LowerOK:   capacity + 1,
				UpperFail: capacity + 5,
				Estimate:  capacity + 9,
			},
			check: func(t *testing.T, l LiquidityInterval) {
				require.Equal(t, capacity, l.LowerOK)
				require.EqualValues(t, 0, l.UpperFail)
				require.Equal(t, capacity, l.Estimate)
			},
		},
		{
			name: "estimate is pulled inside the interval",
			interval: LiquidityInterval{
				LowerOK:   100,
				UpperFail: 1000,
				Estimate:  5000,
			},
			check: func(t *testing.T, l LiquidityInterval) {
				require.EqualValues(t, 999, l.Estimate)
			},
		},
		{
			name: "estimate is raised to the lower bound",
			interval: LiquidityInterval{
				LowerOK:  100,
				Estimate: 10,
			},
			check: func(t *testing.T, l LiquidityInterval) {
				require.EqualValues(t, 100, l.Estimate)
			},
		},
		{
			name: "a low estimate classifies as depleted",
			interval: LiquidityInterval{
				Estimate: capacity / 100,
			},
			check: func(t *testing.T, l LiquidityInterval) {
				require.Equal(
					t, intervalModeDepleted, l.Mode,
				)
			},
		},
		{
			name: "a high estimate classifies as rich",
			interval: LiquidityInterval{
				Estimate: capacity,
			},
			check: func(t *testing.T, l LiquidityInterval) {
				require.Equal(t, intervalModeRich, l.Mode)
			},
		},
		{
			name: "a middling estimate stays unclassified",
			interval: LiquidityInterval{
				Estimate: capacity / 2,
			},
			check: func(t *testing.T, l LiquidityInterval) {
				require.Equal(t, intervalModeUnknown, l.Mode)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			interval := test.interval
			interval.normalize(capacity)

			// The invariant must hold in every case.
			require.LessOrEqual(t, interval.LowerOK,
				interval.Estimate)
			require.LessOrEqual(t, interval.Estimate, capacity)
			if interval.UpperFail != 0 {
				require.Less(t, interval.Estimate,
					interval.UpperFail)
				require.LessOrEqual(t, interval.UpperFail,
					capacity)
			}

			test.check(t, interval)
		})
	}
}

// TestIntervalPrior tests the shape of the bimodal prior: near certainty for
// tiny amounts, a cliff near capacity, and scale invariance across channel
// sizes.
func TestIntervalPrior(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity

	// A dust amount is nearly certain to pass.
	require.Greater(t, intervalPrior(capacity/10000, capacity), 0.95)

	// An amount taking the whole channel is unlikely to pass. The cliff of
	// the saturated mode sits just under capacity rather than at it, so
	// what is left here is the tail of that mode, not zero.
	require.Less(t, intervalPrior(capacity, capacity), 0.1)

	// The prior is monotonically decreasing in the amount.
	previous := 1.0
	for i := 1; i <= 100; i++ {
		amt := capacity / 100 * lnwire.MilliSatoshi(i)
		current := intervalPrior(amt, capacity)
		require.LessOrEqual(t, current, previous)
		previous = current
	}

	// The prior depends on the ratio, not the absolute amount, so the same
	// fraction of a channel a thousand times larger prices the same.
	big := capacity * 1000
	for _, fraction := range []lnwire.MilliSatoshi{2, 5, 10, 100} {
		require.InDelta(
			t, intervalPrior(capacity/fraction, capacity),
			intervalPrior(big/fraction, big), 1e-9,
		)
	}

	// An amount larger than the capacity is impossible.
	require.Zero(t, intervalPrior(capacity+1, capacity))
}

// TestIntervalRetryFactor tests that the retry ladder gives back belief in
// proportion to how much smaller a retry is than the amount that failed.
func TestIntervalRetryFactor(t *testing.T) {
	t.Parallel()

	failedAt := lnwire.MilliSatoshi(1000)

	// Without a failure the factor leaves the probability alone.
	require.EqualValues(t, 1, intervalRetryFactor(500, 0))

	// At or above the failed amount there is no point in trying.
	require.Zero(t, intervalRetryFactor(1000, failedAt))
	require.Zero(t, intervalRetryFactor(1500, failedAt))

	// Below it, the factor rises as the retry shrinks.
	previous := 0.0
	for _, amt := range []lnwire.MilliSatoshi{
		900, 500, 300, 100, 20, 5,
	} {
		current := intervalRetryFactor(amt, failedAt)
		require.Greater(t, current, previous)
		previous = current
	}

	// A retry at a thousandth of the failed amount is nearly unaffected.
	require.EqualValues(
		t, intervalRetryFloor, intervalRetryFactor(1, failedAt),
	)
}

// TestIntervalProbabilityBranches tests that the probability model orders its
// answers by how much the evidence proves.
func TestIntervalProbabilityBranches(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity
	amt := capacity / 2

	// With no capacity the model has no scale to work with and falls back
	// to a flat guess.
	var empty LiquidityInterval
	require.EqualValues(
		t, intervalUnknownCapacity, empty.Probability(amt, 0),
	)

	// With no observations at all the model returns the prior.
	require.Equal(
		t, intervalPrior(amt, capacity),
		empty.Probability(amt, capacity),
	)

	// A proven lower bound is near certainty.
	proven := LiquidityInterval{Known: true, LowerOK: amt}
	require.EqualValues(
		t, intervalProvenProbability,
		proven.Probability(amt, capacity),
	)

	// A proven upper bound is exactly zero, which is the only way the model
	// says impossible.
	failed := LiquidityInterval{Known: true, UpperFail: amt}
	require.Zero(t, failed.Probability(amt, capacity))
	require.Zero(t, failed.Probability(amt+1, capacity))

	// Inside a known interval the probability falls off with position, from
	// near the proven bound to near the failed one.
	interval := LiquidityInterval{
		Known:     true,
		LowerOK:   capacity / 10,
		UpperFail: capacity / 2,
	}
	interval.normalize(capacity)

	low := interval.Probability(capacity/10+1, capacity)
	mid := interval.Probability(capacity/4, capacity)
	high := interval.Probability(capacity/2-1, capacity)

	require.Greater(t, low, mid)
	require.Greater(t, mid, high)
	require.Greater(t, low, 0.5)
	require.Less(t, high, 0.2)

	// Every branch stays inside the clamps.
	for _, l := range []LiquidityInterval{
		empty, proven, failed, interval,
	} {
		for i := 1; i <= 100; i++ {
			p := l.Probability(
				capacity/100*lnwire.MilliSatoshi(i), capacity,
			)
			require.GreaterOrEqual(t, p, 0.0)
			require.LessOrEqual(t, p, intervalMaxProbability)
		}
	}
}

// TestIntervalStoreProbe tests that a forwarded amount raises the forward lower
// bound and bounds the reverse direction, because liquidity on one side of a
// channel is not on the other.
func TestIntervalStoreProbe(t *testing.T) {
	t.Parallel()

	store := NewIntervalStore(0)
	capacity := testIntervalCapacity
	amt := capacity / 4

	store.RecordProbe(testIntervalKey, amt, capacity)

	forward := store.Get(testIntervalKey, capacity)
	require.True(t, forward.Known)
	require.Equal(t, amt, forward.LowerOK)
	require.EqualValues(t, 0, forward.UpperFail)
	require.Equal(t, intervalModeRich, forward.Mode)
	require.EqualValues(t, 1, forward.Successes)

	// The amount we just watched pass is now near certain.
	require.EqualValues(
		t, intervalProvenProbability,
		store.Probability(testIntervalKey, amt, capacity),
	)

	// The reverse direction cannot hold what the forward direction just
	// proved it holds.
	reverse := store.Get(testIntervalKey.Reverse(), capacity)
	require.True(t, reverse.Known)
	require.Equal(t, capacity-amt+1, reverse.UpperFail)
	require.Equal(t, intervalModeDepleted, reverse.Mode)
	require.Zero(t, store.Probability(
		testIntervalKey.Reverse(), capacity-amt+1, capacity,
	))

	// A larger probe raises the bound further.
	store.RecordProbe(testIntervalKey, amt*2, capacity)
	forward = store.Get(testIntervalKey, capacity)
	require.Equal(t, amt*2, forward.LowerOK)

	// A smaller one does not lower it.
	store.RecordProbe(testIntervalKey, amt, capacity)
	forward = store.Get(testIntervalKey, capacity)
	require.Equal(t, amt*2, forward.LowerOK)
}

// TestIntervalStoreFailure tests that a failure drops the forward upper bound
// and is read as evidence of liquidity in the reverse direction.
func TestIntervalStoreFailure(t *testing.T) {
	t.Parallel()

	store := NewIntervalStore(0)
	capacity := testIntervalCapacity
	amt := capacity / 2

	store.RecordFailure(testIntervalKey, amt, capacity)

	forward := store.Get(testIntervalKey, capacity)
	require.Equal(t, amt, forward.UpperFail)
	require.Equal(t, intervalModeDepleted, forward.Mode)
	require.EqualValues(t, 1, forward.Failures)

	// The failing amount and anything above it is now impossible, while a
	// much smaller amount is still worth trying.
	require.Zero(t, store.Probability(testIntervalKey, amt, capacity))
	require.Zero(t, store.Probability(testIntervalKey, amt+1, capacity))
	require.Greater(
		t, store.Probability(testIntervalKey, amt/100, capacity), 0.0,
	)

	// A failure in one direction is evidence of available liquidity in the
	// other, and is counted there as a success.
	reverse := store.Get(testIntervalKey.Reverse(), capacity)
	require.Equal(t, capacity-amt+1, reverse.LowerOK)
	require.Equal(t, intervalModeRich, reverse.Mode)
	require.EqualValues(t, 1, reverse.Successes)

	// A smaller failure tightens the bound, a larger one does not loosen
	// it.
	store.RecordFailure(testIntervalKey, amt/2, capacity)
	require.Equal(t, amt/2, store.Get(testIntervalKey, capacity).UpperFail)

	store.RecordFailure(testIntervalKey, amt, capacity)
	require.Equal(t, amt/2, store.Get(testIntervalKey, capacity).UpperFail)
}

// TestIntervalStoreProbeThenFailure tests that a failure after a success leaves
// a bracketed interval rather than throwing one of the two observations away.
func TestIntervalStoreProbeThenFailure(t *testing.T) {
	t.Parallel()

	store := NewIntervalStore(0)
	capacity := testIntervalCapacity

	store.RecordProbe(testIntervalKey, capacity/10, capacity)
	store.RecordFailure(testIntervalKey, capacity/2, capacity)

	interval := store.Get(testIntervalKey, capacity)
	require.Equal(t, capacity/10, interval.LowerOK)
	require.Equal(t, capacity/2, interval.UpperFail)
	require.Less(t, interval.Estimate, interval.UpperFail)
	require.GreaterOrEqual(t, interval.Estimate, interval.LowerOK)

	// A failure at an amount we have already proven passes cannot leave the
	// interval inverted. The upper bound is what gives way, because the
	// lower one records something we watched succeed.
	store.RecordFailure(testIntervalKey, capacity/20, capacity)

	interval = store.Get(testIntervalKey, capacity)
	if interval.UpperFail != 0 {
		require.Less(t, interval.LowerOK, interval.UpperFail)
	}
}

// TestIntervalStoreSettlement tests that a settlement shifts both directions of
// the interval rather than merely narrowing them, because the balance really
// has moved across the channel.
func TestIntervalStoreSettlement(t *testing.T) {
	t.Parallel()

	store := NewIntervalStore(0)
	capacity := testIntervalCapacity
	amt := capacity / 10

	// Prove a large amount passes, then settle a smaller one over it.
	store.RecordProbe(testIntervalKey, capacity/2, capacity)
	before := store.Get(testIntervalKey, capacity)

	store.RecordSettlement(testIntervalKey, amt, capacity)
	after := store.Get(testIntervalKey, capacity)

	// The forward interval slides down by what left.
	require.Equal(t, before.LowerOK-amt, after.LowerOK)
	require.Equal(t, before.Estimate-amt, after.Estimate)

	// The reverse interval slides up by the same.
	reverse := store.Get(testIntervalKey.Reverse(), capacity)
	require.GreaterOrEqual(t, reverse.LowerOK, amt)
	require.Equal(t, capacity-after.Estimate, reverse.Estimate)

	// A settlement of the whole capacity leaves nothing behind rather than
	// wrapping around.
	require.NoError(t, store.Clear(t.Context()))
	store.RecordSettlement(testIntervalKey, capacity, capacity)

	drained := store.Get(testIntervalKey, capacity)
	require.Zero(t, drained.Estimate)
	require.Zero(t, drained.LowerOK)
	require.Equal(t, intervalModeDepleted, drained.Mode)

	filled := store.Get(testIntervalKey.Reverse(), capacity)
	require.Equal(t, capacity, filled.LowerOK)
	require.Equal(t, intervalModeRich, filled.Mode)
}

// TestIntervalStoreConfidence tests that confidence is a saturating latch that
// only ever rises with evidence.
func TestIntervalStoreConfidence(t *testing.T) {
	t.Parallel()

	store := NewIntervalStore(0)
	capacity := testIntervalCapacity

	require.Zero(t, store.Get(testIntervalKey, capacity).Confidence)

	store.RecordProbe(testIntervalKey, capacity/10, capacity)
	probed := store.Get(testIntervalKey, capacity).Confidence
	require.EqualValues(t, intervalProbeConfidence, probed)

	// A failure carries more weight than a probe, so it raises confidence.
	store.RecordFailure(testIntervalKey, capacity/2, capacity)
	failed := store.Get(testIntervalKey, capacity).Confidence
	require.Greater(t, failed, probed)

	// A settlement latches at a lower level, which must not pull the latch
	// back down.
	store.RecordSettlement(testIntervalKey, capacity/100, capacity)
	settled := store.Get(testIntervalKey, capacity).Confidence
	require.Equal(t, failed, settled)
}

// TestIntervalStoreIgnoresUninformativeObservations tests that observations the
// model cannot use are dropped rather than recorded as something they are not.
func TestIntervalStoreIgnoresUninformativeObservations(t *testing.T) {
	t.Parallel()

	store := NewIntervalStore(0)

	// A zero amount says nothing.
	store.RecordFailure(testIntervalKey, 0, testIntervalCapacity)
	require.Zero(t, store.Len())

	// Neither does an observation about a channel whose size we do not
	// know, since every threshold in the model is a fraction of capacity.
	store.RecordFailure(testIntervalKey, 100, 0)
	require.Zero(t, store.Len())

	// An amount larger than the capacity is clamped rather than dropped,
	// because the capacity we path find against can be synthetic.
	store.RecordFailure(
		testIntervalKey, testIntervalCapacity*2, testIntervalCapacity,
	)
	interval := store.Get(testIntervalKey, testIntervalCapacity)
	require.Equal(t, testIntervalCapacity, interval.UpperFail)
}

// TestIntervalStoreRestoreIsSoft tests the one property a restored belief must
// have. A bound written down before a restart describes a network that has had
// every chance to move on, and this model has no clock and no way to revise a
// bound except by attempting the amount. A restored upper bound that returned
// zero would therefore be permanent: the amount would never be tried again, so
// the evidence that would correct it could never arrive.
func TestIntervalStoreRestoreIsSoft(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity
	amt := capacity / 2

	// Gather a hard bound the ordinary way, and read it back out the way a
	// persistence layer would.
	fresh := NewIntervalStore(0)
	fresh.RecordFailure(testIntervalKey, amt, capacity)
	require.Zero(t, fresh.Probability(testIntervalKey, amt, capacity))

	saved := make(map[IntervalKey]LiquidityInterval)
	fresh.ForEach(func(key IntervalKey, interval LiquidityInterval) {
		saved[key] = interval
	})
	require.Contains(t, saved, testIntervalKey)

	// Hand it to a store that has just started up.
	restored := NewIntervalStore(0)
	for key, interval := range saved {
		restored.Restore(key, interval)
	}

	interval := restored.Get(testIntervalKey, capacity)
	require.True(t, interval.Restored)
	require.Equal(t, saved[testIntervalKey].UpperFail, interval.UpperFail)

	// The bound survived, but it no longer says impossible, so the amount
	// can be tried again and the belief can be corrected.
	probability := restored.Probability(testIntervalKey, amt, capacity)
	require.GreaterOrEqual(t, probability, intervalRestoredFloor)
	require.Less(t, probability, 0.5)

	// It still says the amount is a bad bet, which is the whole point of
	// keeping it: a restored bound outranks having no belief at all.
	require.Less(
		t, probability,
		NewIntervalStore(0).Probability(testIntervalKey, amt, capacity),
	)

	// A restored lower bound is softened from the other side, so a channel
	// that used to carry an amount is not trusted as if we had just watched
	// it do so.
	proven := NewIntervalStore(0)
	proven.Restore(testIntervalKey, LiquidityInterval{
		Known: true, LowerOK: amt, Estimate: amt, Confidence: 1,
	})

	restoredHigh := proven.Probability(testIntervalKey, amt, capacity)
	require.LessOrEqual(t, restoredHigh, intervalRestoredCeiling)
	require.Less(t, restoredHigh, intervalProvenProbability)

	// Confidence is cut, because whatever stood behind the belief is at
	// least one restart old.
	require.Equal(
		t, intervalRestoredConfidence,
		proven.Get(testIntervalKey, capacity).Confidence,
	)
}

// TestIntervalStoreFreshEvidenceBeatsRestored tests that a restored belief
// gives way to an observation this process made, on both sides of the channel.
func TestIntervalStoreFreshEvidenceBeatsRestored(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity
	amt := capacity / 2

	store := NewIntervalStore(0)
	store.Restore(testIntervalKey, LiquidityInterval{
		Known: true, UpperFail: amt, Confidence: 1,
	})
	require.True(t, store.Get(testIntervalKey, capacity).Restored)

	// Watching the channel carry the amount replaces the restored belief
	// outright, certainty and all.
	store.RecordProbe(testIntervalKey, amt, capacity)

	interval := store.Get(testIntervalKey, capacity)
	require.False(t, interval.Restored)
	require.Equal(t, amt, interval.LowerOK)
	require.EqualValues(
		t, intervalProvenProbability,
		store.Probability(testIntervalKey, amt, capacity),
	)

	// The reverse direction was written by the same observation, so it is
	// no longer restored either.
	require.False(t, store.Get(testIntervalKey.Reverse(), capacity).Restored)

	// A restore that arrives after we have seen the channel ourselves is
	// ignored, since what we watched beats what we read back.
	store.Restore(testIntervalKey, LiquidityInterval{
		Known: true, UpperFail: 1,
	})
	require.False(t, store.Get(testIntervalKey, capacity).Restored)
	require.Equal(t, amt, store.Get(testIntervalKey, capacity).LowerOK)
}

// TestIntervalStoreEviction tests that the store stays inside its bound.
func TestIntervalStoreEviction(t *testing.T) {
	t.Parallel()

	const maxEntries = 100

	store := NewIntervalStore(maxEntries)
	capacity := testIntervalCapacity

	for i := 0; i < maxEntries*4; i++ {
		key := IntervalKey{
			ChanID: uint64(i),
			From:   route.Vertex{byte(i), byte(i >> 8)},
			To:     route.Vertex{byte(i >> 8), byte(i)},
		}

		store.RecordFailure(key, capacity/2, capacity)
		require.LessOrEqual(t, store.Len(), maxEntries+2)
	}

	// The most recent observation survived the eviction.
	final := maxEntries*4 - 1
	last := IntervalKey{
		ChanID: uint64(final),
		From:   route.Vertex{byte(final), byte(final >> 8)},
		To:     route.Vertex{byte(final >> 8), byte(final)},
	}
	require.True(t, store.Get(last, capacity).Known)

	require.NoError(t, store.Clear(t.Context()))
	require.Zero(t, store.Len())
}
