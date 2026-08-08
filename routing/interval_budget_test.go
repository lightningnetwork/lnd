package routing

import (
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// The budget tests run over two corridors that differ in exactly two ways: one
// is free and unproven, the other charges a fee and has been watched carry the
// amount. Which one the session picks is then a pure statement about how it
// prices reliability against money.
const (
	// budgetCapacity is the capacity of every channel in the corridors.
	budgetCapacity = btcutil.Amount(1_000_000)

	// budgetAmount is the amount paid, a tenth of a channel.
	budgetAmount = lnwire.MilliSatoshi(100_000_000)

	// budgetHopFee is what the expensive corridor charges to forward.
	budgetHopFee = lnwire.MilliSatoshi(200_000)
)

// newBudgetSession builds a session over a free corridor through the first
// relay and a paying corridor through the second, and proves the paying one by
// recording that it has carried the amount.
//
// The fee limit is the one the payment is created with, which is what decides
// whether the session treats it as budgeted. Handing a limit to RequestRoute is
// not the same thing and deliberately does not classify.
func newBudgetSession(t *testing.T, feeLimit lnwire.MilliSatoshi) (
	*intervalPaymentSession, IntervalKey) {

	t.Helper()

	var (
		source = createPubkey(sourceNodeID)
		cheap  = createPubkey(firstRelayID)
		dear   = createPubkey(secondRelayID)
		target = createPubkey(targetNodeID)
	)

	graph := &parallelGraph{
		channels: []parallelChannel{
			{
				id: 1, node1: source, node2: cheap,
				capacity: budgetCapacity,
			},
			{
				id: 2, node1: cheap, node2: target,
				capacity: budgetCapacity,
			},
			{
				id: 3, node1: source, node2: dear,
				capacity: budgetCapacity,
			},
			{
				id: 4, node1: dear, node2: target,
				capacity: budgetCapacity, baseFee: budgetHopFee,
			},
		},
	}

	var paymentAddr [32]byte
	payment := &LightningPayment{
		FinalCLTVDelta: 40,
		FeeLimit:       feeLimit,
		Target:         target,
		PaymentAddr:    fn.Some(paymentAddr),
		Amount:         budgetAmount,
		CltvLimit:      math.MaxUint32,
		MaxParts:       1,
		DestFeatures: lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadOptional,
				lnwire.PaymentAddrOptional,
				lnwire.MPPOptional,
			), lnwire.Features,
		),
	}
	require.NoError(t, payment.SetPaymentHash([32]byte{}))

	getBandwidthHints := func(_ Graph) (bandwidthHints, error) {
		return &mockBandwidthHints{
			hints: map[uint64]lnwire.MilliSatoshi{
				1: lnwire.NewMSatFromSatoshis(budgetCapacity),
				3: lnwire.NewMSatFromSatoshis(budgetCapacity),
			},
		}, nil
	}

	store := NewIntervalStore(0)

	// The paying corridor's interior hop has been watched carry the amount,
	// so it is near certain where the free corridor is only a guess.
	proven := IntervalKey{ChanID: 4, From: dear, To: target}
	store.RecordProbe(
		proven, budgetAmount,
		lnwire.NewMSatFromSatoshis(budgetCapacity),
	)

	session, err := newIntervalPaymentSession(
		payment, source, getBandwidthHints, graph, store,
		DefaultIntervalConfig(),
	)
	require.NoError(t, err)

	return session, proven
}

// TestIntervalBudgetPrice tests the exchange rate that decides whether a fee
// budget can influence the search at all. The units are the finding here: a
// rate proportional to the amount can never be reached by a realistic budget,
// while an absolute rate can.
func TestIntervalBudgetPrice(t *testing.T) {
	t.Parallel()

	amt := lnwire.MilliSatoshi(1_000_000_000)

	// A payment with no budget has no rate at all.
	require.Zero(t, newIntervalFeeRate(false, amt).price)
	require.False(t, newIntervalFeeRate(false, amt).budgeted)

	// With a budget the rate is absolute and derived from what is left.
	budget := lnwire.MilliSatoshi(400_000)
	require.Equal(
		t, float64(budget)/intervalBudgetShare,
		intervalBudgetPrice(budget),
	)

	// The rate falls as the budget is spent, so a payment running low
	// prices reliability ever more cheaply and stops paying up for it.
	previous := math.MaxFloat64
	for _, left := range []lnwire.MilliSatoshi{
		800_000, 400_000, 200_000, 120_000,
	} {
		current := intervalBudgetPrice(left)
		require.Less(t, current, previous)
		previous = current
	}

	// The rate is bounded at both ends. A payment with almost nothing left
	// still pays something, since a route it can afford beats no route.
	require.Equal(t, intervalMinFeePrice, intervalBudgetPrice(1))
	require.Equal(
		t, intervalMaxFeePrice,
		intervalBudgetPrice(lnwire.MaxMilliSatoshi-1),
	)

	// Because the rate is absolute, its ceiling in relative terms tightens
	// as the payment grows, which is the direction a budget quoted in parts
	// per million needs.
	rate := intervalBudgetPrice(budget)
	require.Greater(
		t, rate/float64(lnwire.MilliSatoshi(1_000_000)),
		rate/float64(amt),
	)

	// Read as a price, the unbudgeted fallback is a fifth of the payment,
	// which no fee budget anybody would set comes close to. That is the
	// whole reason it never binds.
	unbudgeted := intervalFeeRate{}.penalty(1, amt, intervalFeeWeight)
	require.Less(t, unbudgeted, 1/(float64(amt)/10))
}

// TestIntervalFeePenaltyUnbudgetedIsVerbatim tests that the fee term of an
// unbudgeted payment is bit for bit the expression it has always been.
//
// This is a stricter test than it looks. The obvious refactor, precomputing
// the amount over the weight and dividing the fee by that, is algebraically
// the same and not the same in floating point, because it rounds twice where
// the original rounds once. The frontier compares these scores exactly, so a
// last-bit difference reorders labels and returns a different route. It was
// measured at 0.032 of objective on one tier. Equality here is therefore
// asserted on the bits, and the second half proves the assertion can fail.
func TestIntervalFeePenaltyUnbudgetedIsVerbatim(t *testing.T) {
	t.Parallel()

	amounts := []lnwire.MilliSatoshi{
		0, 1, 4, 5, 6, 1_000_000, 7_000_003, 33_333_337,
		100_000_000, 123_456_789, 200_000_000,
	}
	fees := []lnwire.MilliSatoshi{
		0, 1, 3, 997, 100_000, 262_144, 499_999, 500_000,
	}
	weights := []float64{intervalFeeWeight, intervalShardFeeWeight}

	// reciprocal is the form this must never be written as.
	reciprocal := func(fee float64, amt lnwire.MilliSatoshi,
		weight float64) float64 {

		return fee / math.Max(float64(amt)/weight, 1)
	}

	var differs int
	for _, weight := range weights {
		for _, amt := range amounts {
			for _, feeAmt := range fees {
				fee := float64(feeAmt)

				want := weight * fee /
					math.Max(float64(amt), 1)
				got := intervalFeeRate{}.penalty(
					fee, amt, weight,
				)

				require.Equal(t, math.Float64bits(want),
					math.Float64bits(got),
					"amt=%v fee=%v weight=%v", amt, feeAmt,
					weight)

				if reciprocal(fee, amt, weight) != want {
					differs++
				}
			}
		}
	}

	// The reciprocal form disagrees on a good fraction of these, which is
	// what makes the equality above worth asserting rather than a ritual.
	require.NotZero(t, differs, "the sweep found no case where the "+
		"reciprocal form differs, so it cannot catch the regression")

	// A payment with a budget takes the other branch and pays the rate the
	// budget sets.
	rate := newIntervalFeeRate(true, 400_000)
	require.Equal(
		t, 1_000/rate.price,
		rate.penalty(1_000, 100_000_000, intervalFeeWeight),
	)
}

// TestIntervalBudgetPicksCheapCorridor tests that a binding budget changes the
// route. With money effectively free the session buys the reliability it has
// evidence for, and with a budget tight enough that the same reliability costs
// more than it is worth, the session takes the cheap corridor instead.
func TestIntervalBudgetPicksCheapCorridor(t *testing.T) {
	t.Parallel()

	var (
		cheap = createPubkey(firstRelayID)
		dear  = createPubkey(secondRelayID)
	)

	// With no budget the fee term is a rounding error against the risk of
	// an unproven corridor, so the proven one wins.
	session, _ := newBudgetSession(t, lnwire.MaxMilliSatoshi)

	rt, err := session.RequestRoute(
		budgetAmount, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)
	require.Equal(t, dear, rt.Hops[0].PubKeyBytes)
	require.EqualValues(t, budgetHopFee, rt.TotalAmount-budgetAmount)

	// Now hand the same session the same choice with a budget that can
	// still afford the paying corridor twice over, but under which one nat
	// of reliability is no longer worth what that corridor charges.
	session, _ = newBudgetSession(t, budgetHopFee*2)

	rt, err = session.RequestRoute(budgetAmount, budgetHopFee*2, 0, 0, nil)
	require.NoError(t, err)
	require.Equal(t, cheap, rt.Hops[0].PubKeyBytes)
	require.Zero(t, rt.TotalAmount-budgetAmount)
}

// TestIntervalBudgetNeverExceeded tests the discipline lnd's own session has
// and this one must match: no route is ever returned that the payment cannot
// afford, at any budget.
func TestIntervalBudgetNeverExceeded(t *testing.T) {
	t.Parallel()

	limits := []lnwire.MilliSatoshi{
		lnwire.MaxMilliSatoshi, budgetHopFee * 4, budgetHopFee * 2,
		budgetHopFee, budgetHopFee - 1, budgetHopFee / 2, 1,
	}

	for _, limit := range limits {
		session, _ := newBudgetSession(t, limit)

		rt, err := session.RequestRoute(budgetAmount, limit, 0, 0, nil)
		if err != nil {
			require.ErrorIs(t, err, errNoPathFound)

			continue
		}

		fee := rt.TotalAmount - budgetAmount
		require.LessOrEqual(t, fee, limit,
			"returned a route costing %v under a limit of %v",
			fee, limit)
	}

	// A budget too small for even the free corridor's zero fee is still
	// routable, since the free corridor costs nothing.
	session, _ := newBudgetSession(t, 0)
	rt, err := session.RequestRoute(budgetAmount, 0, 0, 0, nil)
	require.NoError(t, err)
	require.Zero(t, rt.TotalAmount-budgetAmount)
}

// TestIntervalFrontierKeepsCheapestLabel tests that the cheapest way out of a
// node is protected from eviction when the payment carries a fee budget, and
// only then.
//
// Under a budget the protection is what stops a frontier of reliable expensive
// labels from leaving a payment that cannot afford any of them with nothing.
// Without a budget it is a label kept for a limit that does not exist,
// displacing one that would have served the payment being made, and measurement
// found that costs real success on payments with no limit set.
func TestIntervalFrontierKeepsCheapestLabel(t *testing.T) {
	t.Parallel()

	const deliver = lnwire.MilliSatoshi(1_000_000)

	// fill builds a frontier holding one cheap badly scoring label plus
	// enough better scoring dearer ones to force eviction, and returns the
	// cheap label and what the node ended up keeping.
	fill := func(keepCheapest bool) (*intervalLabel, []*intervalLabel) {
		node := route.Vertex{1}
		frontier := &intervalFrontier{
			labels:       map[route.Vertex][]*intervalLabel{},
			maxLabels:    3,
			keepCheapest: keepCheapest,
		}

		// The cheapest label is also the worst scoring one, so nothing
		// but the protection would keep it.
		cheapest := &intervalLabel{
			node:              node,
			netAmountReceived: deliver,
			score:             100,
			hops:              1,
		}
		require.True(t, frontier.insert(cheapest, deliver))

		// Score falls as the amount rises across the rest of the set, so
		// no label dominates another and each is a genuine trade-off the
		// search would want to keep.
		for i := 1; i <= 6; i++ {
			frontier.insert(&intervalLabel{
				node: node,
				netAmountReceived: deliver *
					lnwire.MilliSatoshi(10+i),
				score: float64(10 - i),
				hops:  1,
			}, deliver)
		}

		kept := frontier.labels[node]
		require.Len(t, kept, frontier.maxLabels)

		return cheapest, kept
	}

	// With a budget the cheap label survives, and it is still the cheapest
	// thing the node holds.
	cheapest, kept := fill(true)

	require.Contains(t, kept, cheapest)
	require.True(t, cheapest.active)

	for _, label := range kept {
		require.GreaterOrEqual(
			t, label.netAmountReceived,
			cheapest.netAmountReceived,
		)
	}

	// With no budget it is evicted on its score like any other label, which
	// is the behaviour the search had before fee budgets were priced at all.
	cheapest, kept = fill(false)

	require.NotContains(t, kept, cheapest)
	require.False(t, cheapest.active)

	for _, label := range kept {
		require.Less(t, label.score, cheapest.score)
	}
}

// TestIntervalBudgeted tests the switch that decides both how fees are priced
// and which of the two eviction rules a search uses. Anything short of the
// sentinel is a real limit a route can exceed, so anything short of it counts.
func TestIntervalBudgeted(t *testing.T) {
	t.Parallel()

	require.False(t, intervalBudgeted(lnwire.MaxMilliSatoshi))

	for _, limit := range []lnwire.MilliSatoshi{
		0, 1, budgetHopFee, lnwire.MaxMilliSatoshi - 1,
	} {
		require.True(t, intervalBudgeted(limit),
			"a limit of %v should price fees", limit)
	}
}

// TestIntervalBudgetSurvivesShards tests the bug this classification exists to
// avoid, and its mirror.
//
// RequestRoute is handed the budget remaining, not the budget. An unbudgeted
// payment therefore carries the no-limit sentinel only until its first shard
// pays a fee; from the second shard on it carries the sentinel minus that fee,
// which is an ordinary looking number. Reading that as "this payment has a
// budget" flipped every unbudgeted payment that splits onto the budgeted
// branch, priced its fees against a limit nobody set, and turned on a frontier
// protection it had no use for.
func TestIntervalBudgetSurvivesShards(t *testing.T) {
	t.Parallel()

	// What the lifecycle hands the second route request of a payment whose
	// first shard paid a fee. It is not the sentinel, and that is the point.
	const feesPaid = budgetHopFee

	afterFirstShard := lnwire.MaxMilliSatoshi - feesPaid
	require.NotEqual(t, lnwire.MaxMilliSatoshi, afterFirstShard)

	// A payment created with no limit stays unbudgeted across its shards,
	// however much of the sentinel its shards have eaten.
	session, _ := newBudgetSession(t, lnwire.MaxMilliSatoshi)
	require.False(t, session.budgeted)

	for _, remaining := range []lnwire.MilliSatoshi{
		lnwire.MaxMilliSatoshi, afterFirstShard,
		lnwire.MaxMilliSatoshi - feesPaid*2, budgetHopFee,
	} {
		rate := session.feeRate(remaining)

		// No budget branch, so no budget price, no cheapest-label keep,
		// and a fee term that is the verbatim amount relative
		// expression.
		require.False(t, rate.budgeted,
			"reclassified as budgeted at a remainder of %v",
			remaining)
		require.Zero(t, rate.price)
		require.Equal(
			t, intervalFeeWeight*float64(budgetHopFee)/
				math.Max(float64(budgetAmount), 1),
			rate.penalty(
				float64(budgetHopFee), budgetAmount,
				intervalFeeWeight,
			),
		)
	}

	// Driving two requests through the session leaves the latch alone, which
	// is the whole of the fix.
	_, err := session.RequestRoute(
		budgetAmount, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)
	require.False(t, session.budgeted)

	_, err = session.RequestRoute(budgetAmount, afterFirstShard, 0, 0, nil)
	require.NoError(t, err)
	require.False(t, session.budgeted)
	require.False(t, session.feeRate(afterFirstShard).budgeted)

	// The mirror: a payment created with a limit stays budgeted across its
	// shards, and the rate it pays follows the shrinking remainder down.
	budgeted, _ := newBudgetSession(t, budgetHopFee*4)
	require.True(t, budgeted.budgeted)

	// These remainders are all under the rate ceiling, so the rate they
	// produce actually moves rather than pinning to the clamp.
	previous := math.MaxFloat64
	for _, remaining := range []lnwire.MilliSatoshi{
		budgetHopFee * 4, budgetHopFee * 3, budgetHopFee * 2,
	} {
		rate := budgeted.feeRate(remaining)

		require.True(t, rate.budgeted)
		require.Equal(t, intervalBudgetPrice(remaining), rate.price)
		require.Less(t, rate.price, previous)
		previous = rate.price

		// The budgeted branch prices off the rate rather than off the
		// amount.
		require.Equal(
			t, float64(budgetHopFee)/rate.price,
			rate.penalty(
				float64(budgetHopFee), budgetAmount,
				intervalFeeWeight,
			),
		)
	}

	_, err = budgeted.RequestRoute(budgetAmount, budgetHopFee*4, 0, 0, nil)
	require.NoError(t, err)
	require.True(t, budgeted.budgeted)
	require.True(t, budgeted.feeRate(budgetHopFee*2).budgeted)
}
