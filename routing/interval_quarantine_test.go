package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestIntervalQuarantinePricesSoftly tests the property that separates a
// quarantined observation from a bound. A failure we cannot attribute makes an
// amount less attractive and never makes it impossible, because an impossible
// amount is never attempted and an attempt is the only thing that could show
// the suspicion was misplaced.
func TestIntervalQuarantinePricesSoftly(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity
	amt := capacity / 2

	store := NewIntervalStore(0)
	clean := store.Probability(testIntervalKey, amt, capacity)

	// One ambiguous failure naming three channels, so a third of the blame
	// lands here.
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0/3)

	interval := store.Get(testIntervalKey, capacity)
	require.Equal(t, amt, interval.SuspectAmt)
	require.InDelta(t, 1.0/3, interval.SuspectWeight, 1e-9)

	// The bounds are untouched, which is the whole point of the quarantine.
	require.Zero(t, interval.UpperFail)

	// The amount is discounted but still reachable.
	suspected := store.Probability(testIntervalKey, amt, capacity)
	require.Less(t, suspected, clean)
	require.Greater(t, suspected, 0.0)

	// Only the amount the failure named, and larger ones, are discounted. A
	// smaller amount is untouched, since nothing was said against it.
	require.Equal(
		t, store.Probability(testIntervalKey, amt/4, capacity),
		NewIntervalStore(0).Probability(testIntervalKey, amt/4, capacity),
	)
	require.Less(
		t, store.Probability(testIntervalKey, amt*3/2, capacity),
		clean,
	)

	// More agreement discounts harder, without ever reaching zero.
	previous := suspected
	for i := 0; i < 3; i++ {
		store.RecordSuspectFailure(
			testIntervalKey, amt, capacity, 1.0/3,
		)

		current := store.Probability(testIntervalKey, amt, capacity)
		if store.Get(testIntervalKey, capacity).SuspectAmt == 0 {
			// Promoted, which the next test covers.
			break
		}

		require.LessOrEqual(t, current, previous)
		require.Greater(t, current, 0.0)
		previous = current
	}
}

// TestIntervalQuarantinePromotes tests that agreement convicts. Enough
// independent failures naming the same channel and the same amount turn the
// suspicion into an ordinary upper bound, at which point it prices like any
// other thing we have watched fail.
func TestIntervalQuarantinePromotes(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity
	amt := capacity / 2

	store := NewIntervalStore(0)

	// Failures naming two suspects each, so half the blame lands here every
	// time. Three of them clear the promotion threshold.
	for i := 0; i < 2; i++ {
		store.RecordSuspectFailure(testIntervalKey, amt, capacity, 0.5)

		require.NotZero(
			t, store.Get(testIntervalKey, capacity).SuspectAmt,
			"convicted on %d reports", i+1,
		)
		require.Zero(t, store.Get(testIntervalKey, capacity).UpperFail)
	}

	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.05)

	// Convicted. The suspicion is now a bound, and the quarantine that held
	// it is empty, since from here the bound is what speaks.
	interval := store.Get(testIntervalKey, capacity)
	require.Equal(t, amt, interval.UpperFail)
	require.Zero(t, interval.SuspectAmt)
	require.Zero(t, interval.SuspectWeight)
	require.Less(t, interval.Estimate, amt)

	// And it prices as a bound does.
	require.Zero(t, store.Probability(testIntervalKey, amt, capacity))
}

// TestIntervalQuarantinePromotesAtSmallestAmount tests that the quarantine
// keeps the tightest amount it has been shown, so that a conviction bounds the
// channel where the evidence actually put it.
func TestIntervalQuarantinePromotesAtSmallestAmount(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity

	store := NewIntervalStore(0)
	store.RecordSuspectFailure(testIntervalKey, capacity/2, capacity, 1.0)
	store.RecordSuspectFailure(testIntervalKey, capacity/4, capacity, 1.0)

	require.Equal(
		t, capacity/4, store.Get(testIntervalKey, capacity).SuspectAmt,
	)

	store.RecordSuspectFailure(testIntervalKey, capacity/2, capacity, 1.0)

	require.Equal(
		t, capacity/4, store.Get(testIntervalKey, capacity).UpperFail,
	)
}

// TestIntervalQuarantineClearsOnContradiction tests what does and does not
// count as proof that a suspicion was misplaced.
//
// Only a settlement counts. A lower bound is not enough, because a lower bound
// also rises when some hop reports a failure and we infer that the hops before
// it forwarded. That inference is sound exactly when the report names the right
// hop, and misattribution is the case where it does not.
func TestIntervalQuarantineClearsOnContradiction(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity
	amt := capacity / 2

	// A probe does not clear a suspicion, however large. It is an inference
	// from somebody else's failure report, and the report may have named
	// the wrong hop.
	store := NewIntervalStore(0)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0)
	store.RecordProbe(testIntervalKey, amt*3/2, capacity)

	interval := store.Get(testIntervalKey, capacity)
	require.Equal(t, amt, interval.SuspectAmt)
	require.NotZero(t, interval.SuspectWeight)
	require.Zero(t, interval.ProvenOK)

	// A settlement of the suspected amount does clear it. The money moved,
	// which is the one thing no misattribution can manufacture.
	store = NewIntervalStore(0)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0)
	store.RecordSettlement(testIntervalKey, amt, capacity)

	interval = store.Get(testIntervalKey, capacity)
	require.Zero(t, interval.SuspectAmt)
	require.Zero(t, interval.SuspectWeight)
	require.Equal(t, amt, interval.ProvenOK)

	// So does a settlement of more than the suspected amount.
	store = NewIntervalStore(0)
	store.RecordSuspectFailure(testIntervalKey, amt/2, capacity, 1.0)
	store.RecordSettlement(testIntervalKey, amt, capacity)
	require.Zero(t, store.Get(testIntervalKey, capacity).SuspectAmt)

	// A settlement of less does not, since it says nothing about the amount
	// the failure named.
	store = NewIntervalStore(0)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0)
	store.RecordSettlement(testIntervalKey, amt/4, capacity)
	require.Equal(
		t, amt, store.Get(testIntervalKey, capacity).SuspectAmt,
	)

	// A suspicion about an amount we have watched settle is never held in
	// the first place.
	store = NewIntervalStore(0)
	store.RecordSettlement(testIntervalKey, amt, capacity)
	store.RecordSuspectFailure(testIntervalKey, amt/2, capacity, 1.0)
	require.Zero(t, store.Get(testIntervalKey, capacity).SuspectAmt)

	// Proof is monotone: a later, smaller settlement does not walk it back
	// and re-arm a suspicion the larger one had cleared.
	store.RecordSettlement(testIntervalKey, amt/8, capacity)
	require.Equal(t, amt, store.Get(testIntervalKey, capacity).ProvenOK)
}

// TestIntervalQuarantineSurvivesMisattribution tests the failure shape this
// trust boundary exists for.
//
// A failure reported by some hop makes us write a lower bound on every hop
// before it, because forwarding is what got the payment that far. Under
// attribution shift the report names a hop downstream of the one that actually
// refused, which puts the guilty channel before the reported index and hands it
// a lower bound claiming it carried the very amount it just turned down. If
// that bound counted as proof of innocence, the culprit would be struck off
// every suspect list it belonged on, and the weight of the failure would
// concentrate on the innocent channels that remained.
func TestIntervalQuarantineSurvivesMisattribution(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity
	amt := capacity / 2

	store := NewIntervalStore(0)

	// The shifted report: the true culprit is named as a hop that forwarded,
	// so it collects a lower bound for the amount it actually refused.
	store.RecordProbe(testIntervalKey, amt, capacity)
	require.Equal(t, amt, store.Get(testIntervalKey, capacity).LowerOK)
	require.Zero(t, store.Get(testIntervalKey, capacity).ProvenOK)

	// A later ambiguous failure names the same channel at the same amount.
	// The false bound must not suppress it.
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0)

	interval := store.Get(testIntervalKey, capacity)
	require.Equal(t, amt, interval.SuspectAmt,
		"a probe derived bound suppressed a suspicion")
	require.NotZero(t, interval.SuspectWeight)

	// The suspicion prices, so the channel is discounted at the amount it
	// keeps being blamed for.
	clean := NewIntervalStore(0)
	clean.RecordProbe(testIntervalKey, amt, capacity)
	require.Less(
		t, store.Probability(testIntervalKey, amt, capacity),
		clean.Probability(testIntervalKey, amt, capacity),
	)

	// Corroboration still convicts. The false bound sits below the amount
	// the ambiguous failures name, which is the ordinary case, and the
	// promotion writes the bound it should.
	store = NewIntervalStore(0)
	store.RecordProbe(testIntervalKey, amt/2, capacity)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.05)

	interval = store.Get(testIntervalKey, capacity)
	require.Equal(t, amt, interval.UpperFail)
	require.Zero(t, interval.SuspectAmt)

	// One case is worth pinning because it is left deliberately alone. When
	// a false bound lands at exactly the amount the failures name, the
	// promotion is written and then dropped again by the rule that a lower
	// bound and an upper bound at the same amount cannot both stand. That
	// rule is ordinary bound maintenance, it is not part of the quarantine,
	// and rewriting it would be a change nobody has measured. The suspicion
	// is still held and still priced up to that point, which is the part
	// that matters.
	store = NewIntervalStore(0)
	store.RecordProbe(testIntervalKey, amt, capacity)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.05)

	require.Zero(t, store.Get(testIntervalKey, capacity).UpperFail)

	// Ground truth still speaks. A settlement over the same channel clears
	// a suspicion that a hundred probes could not.
	store = NewIntervalStore(0)
	store.RecordProbe(testIntervalKey, amt, capacity)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0)
	require.NotZero(t, store.Get(testIntervalKey, capacity).SuspectAmt)

	store.RecordSettlement(testIntervalKey, amt, capacity)
	require.Zero(t, store.Get(testIntervalKey, capacity).SuspectAmt)
}

// TestIntervalQuarantineSuspectListIgnoresProbes tests the same boundary at the
// place the session applies it: a hop is struck off the suspect list of an
// unattributable failure only when a settlement has proven it, never when a
// probe has merely implied it.
func TestIntervalQuarantineSuspectListIgnoresProbes(t *testing.T) {
	t.Parallel()

	capacity := lnwire.NewMSatFromSatoshis(budgetCapacity)
	amt := lnwire.MilliSatoshi(600_000_000)

	// A route with two hops that are not ours, so an unattributable failure
	// over it has two suspects.
	rt := &route.Route{
		TotalAmount:  amt,
		SourcePubKey: createPubkey(sourceNodeID),
		Hops: []*route.Hop{
			{
				PubKeyBytes:  createPubkey(firstRelayID),
				ChannelID:    1,
				AmtToForward: amt,
			},
			{
				PubKeyBytes:  createPubkey(secondRelayID),
				ChannelID:    9,
				AmtToForward: amt,
			},
			{
				PubKeyBytes:  createPubkey(targetNodeID),
				ChannelID:    4,
				AmtToForward: amt,
			},
		},
	}

	suspects := []IntervalKey{
		{
			ChanID: 9,
			From:   createPubkey(firstRelayID),
			To:     createPubkey(secondRelayID),
		},
		{
			ChanID: 4,
			From:   createPubkey(secondRelayID),
			To:     createPubkey(targetNodeID),
		},
	}

	report := func(prove bool) *IntervalStore {
		session, store := newCorridorSession(
			t, lnwire.NewMSatFromSatoshis(600_000), 1,
		)
		for _, key := range intervalRouteKeys(rt) {
			session.capacities[key] = capacity
		}

		// Both hops carry a probe derived lower bound covering the
		// amount, which is what a shifted report leaves behind.
		for _, key := range suspects {
			store.RecordProbe(key, amt, capacity)
		}

		// One of them additionally has a settlement behind it.
		if prove {
			store.RecordSettlement(suspects[0], amt, capacity)
		}

		session.ReportAttemptFailure(0, rt, nil, nil)

		return store
	}

	// With only probes behind them, both hops stay on the list and both
	// take a share of the suspicion.
	store := report(false)
	for _, key := range suspects {
		require.NotZero(t, store.Get(key, capacity).SuspectAmt,
			"channel %v was struck off on a probe", key.ChanID)
	}

	// With a settlement behind the first, it is struck off, and being the
	// only suspect left makes the second a certainty by elimination rather
	// than a suspicion.
	store = report(true)
	require.Zero(t, store.Get(suspects[0], capacity).SuspectAmt)
	require.Zero(t, store.Get(suspects[1], capacity).SuspectAmt)
	require.NotZero(t, store.Get(suspects[1], capacity).UpperFail)
}

// TestIntervalQuarantineSubsumedByBound tests that a failure we do trust
// swallows a suspicion reaching for the same thing, so that the two do not
// discount the same amount twice over.
func TestIntervalQuarantineSubsumedByBound(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity
	amt := capacity / 2

	store := NewIntervalStore(0)
	store.RecordSuspectFailure(testIntervalKey, amt, capacity, 1.0)
	store.RecordFailure(testIntervalKey, amt/2, capacity)

	interval := store.Get(testIntervalKey, capacity)
	require.Equal(t, amt/2, interval.UpperFail)
	require.Zero(t, interval.SuspectAmt)
}

// TestIntervalQuarantineWritesOneDirection tests that an ambiguous failure says
// nothing about the other side of the channel. The inference that liquidity
// missing here is liquidity present there only holds when we know the failure
// happened here.
func TestIntervalQuarantineWritesOneDirection(t *testing.T) {
	t.Parallel()

	capacity := testIntervalCapacity

	store := NewIntervalStore(0)
	store.RecordSuspectFailure(
		testIntervalKey, capacity/2, capacity, 1.0,
	)

	reverse := store.Get(testIntervalKey.Reverse(), capacity)
	require.False(t, reverse.Known)
	require.Zero(t, reverse.LowerOK)
	require.Zero(t, reverse.SuspectAmt)
}

// TestIntervalQuarantineIgnoresUninformative tests that a quarantine entry is
// only made when there is something to record.
func TestIntervalQuarantineIgnoresUninformative(t *testing.T) {
	t.Parallel()

	store := NewIntervalStore(0)

	store.RecordSuspectFailure(testIntervalKey, 0, testIntervalCapacity, 1)
	store.RecordSuspectFailure(testIntervalKey, 100, 0, 1)
	store.RecordSuspectFailure(testIntervalKey, 100, testIntervalCapacity, 0)

	require.Zero(t, store.Len())
}

// TestIntervalSessionQuarantinesAmbiguousFailure tests the path a real payment
// takes. A failure nobody claims, over a route with several plausible culprits,
// leaves a discount on each of them and a bound on none.
func TestIntervalSessionQuarantinesAmbiguousFailure(t *testing.T) {
	t.Parallel()

	const capacitySat = 100_000

	graph := newIntervalTestGraph(t, []byte{firstRelayID}, capacitySat)
	amt := lnwire.NewMSatFromSatoshis(40_000)
	ctx := newIntervalTestCtx(t, graph, amt, 1, false)

	rt, err := ctx.session.RequestRoute(
		amt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)
	require.Len(t, rt.Hops, 2)

	// A failure with no source and no message, which is what an unreadable
	// onion error looks like by the time it reaches us.
	ctx.session.ReportAttemptFailure(0, rt, nil, nil)

	// Our own first hop is never a suspect, so the only channel that could
	// be blamed is the interior one, and a single suspect is an elimination
	// rather than a guess: that one is bounded outright.
	interior := IntervalKey{
		ChanID: 2,
		From:   createPubkey(firstRelayID),
		To:     createPubkey(targetNodeID),
	}
	capacity := lnwire.NewMSatFromSatoshis(capacitySat)
	require.NotZero(t, ctx.store.Get(interior, capacity).UpperFail)

	// Now the ambiguous case, over a route with two channels neither of
	// which is ours.
	session, store := newCorridorSession(
		t, lnwire.NewMSatFromSatoshis(600_000), 1,
	)
	longRoute := &route.Route{
		TotalAmount:  600_000_000,
		SourcePubKey: createPubkey(sourceNodeID),
		Hops: []*route.Hop{
			{
				PubKeyBytes:  createPubkey(firstRelayID),
				ChannelID:    1,
				AmtToForward: 600_000_000,
			},
			{
				PubKeyBytes:  createPubkey(secondRelayID),
				ChannelID:    9,
				AmtToForward: 600_000_000,
			},
			{
				PubKeyBytes:  createPubkey(targetNodeID),
				ChannelID:    4,
				AmtToForward: 600_000_000,
			},
		},
	}

	// The session has to know the capacities to record anything, which it
	// normally learns while path finding.
	capacity = lnwire.NewMSatFromSatoshis(budgetCapacity)
	for _, key := range intervalRouteKeys(longRoute) {
		session.capacities[key] = capacity
	}

	session.ReportAttemptFailure(0, longRoute, nil, nil)

	// Two suspects, so each carries a quarantined discount and neither
	// carries a bound.
	suspects := []IntervalKey{
		{
			ChanID: 9,
			From:   createPubkey(firstRelayID),
			To:     createPubkey(secondRelayID),
		},
		{
			ChanID: 4,
			From:   createPubkey(secondRelayID),
			To:     createPubkey(targetNodeID),
		},
	}

	for _, key := range suspects {
		interval := store.Get(key, capacity)

		require.NotZero(t, interval.SuspectAmt, "no suspicion on %v",
			key.ChanID)
		require.Zero(t, interval.UpperFail, "bound placed on %v by an "+
			"ambiguous failure", key.ChanID)
		require.Greater(
			t, store.Probability(key, 600_000_000, capacity), 0.0,
		)
	}
}

// TestIntervalQuarantineSeverable tests that the quarantine can be switched off
// without touching anything else. It measured as a null on the tiers built to
// reward it, so whether it ships is a decision somebody should be able to make
// with a config field rather than a patch.
func TestIntervalQuarantineSeverable(t *testing.T) {
	t.Parallel()

	// The zero value keeps the mechanism on, which is the behaviour every
	// published measurement of this router was taken with.
	require.False(t, IntervalConfig{}.DisableQuarantine)
	require.False(t, DefaultIntervalConfig().DisableQuarantine)

	route := func(disabled bool) (*IntervalStore, []IntervalKey) {
		session, store := newCorridorSession(
			t, lnwire.NewMSatFromSatoshis(600_000), 1,
		)
		session.cfg.DisableQuarantine = disabled

		// A route with two hops that are not ours, so an unattributable
		// failure over it has two suspects and neither can be named.
		rt := &route.Route{
			TotalAmount:  600_000_000,
			SourcePubKey: createPubkey(sourceNodeID),
			Hops: []*route.Hop{
				{
					PubKeyBytes:  createPubkey(firstRelayID),
					ChannelID:    1,
					AmtToForward: 600_000_000,
				},
				{
					PubKeyBytes: createPubkey(
						secondRelayID,
					),
					ChannelID:    9,
					AmtToForward: 600_000_000,
				},
				{
					PubKeyBytes:  createPubkey(targetNodeID),
					ChannelID:    4,
					AmtToForward: 600_000_000,
				},
			},
		}

		keys := intervalRouteKeys(rt)
		for _, key := range keys {
			session.capacities[key] = lnwire.NewMSatFromSatoshis(
				budgetCapacity,
			)
		}

		session.ReportAttemptFailure(0, rt, nil, nil)

		return store, keys
	}

	capacity := lnwire.NewMSatFromSatoshis(budgetCapacity)

	// On, the suspects carry a discount.
	store, keys := route(false)

	var suspected int
	for _, key := range keys {
		if store.Get(key, capacity).SuspectAmt != 0 {
			suspected++
		}
	}
	require.NotZero(t, suspected)

	// Off, the store hears nothing at all. Nothing is recorded, so nothing
	// prices, and the payment falls back to handling the failure with the
	// penalties that live and die with it.
	store, keys = route(true)

	require.Zero(t, store.Len())
	for _, key := range keys {
		interval := store.Get(key, capacity)

		require.Zero(t, interval.SuspectAmt)
		require.Zero(t, interval.SuspectWeight)
		require.False(t, interval.Known)
	}
}
