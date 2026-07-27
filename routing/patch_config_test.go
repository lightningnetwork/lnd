package routing

import (
	"os"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// patchTestAmt is the amount every split test asks for, chosen well above the
// default minimum shard amount so that there is an interval to search.
const patchTestAmt = lnwire.MilliSatoshi(1_000_000_000)

// newPatchTestSession builds a payment session for an mpp-capable payment of
// patchTestAmt, with the given patch config and mission control.
func newPatchTestSession(t *testing.T, patch PatchConfig,
	mc MissionControlQuerier) *paymentSession {

	t.Helper()

	var paymentAddr [32]byte
	payment := &LightningPayment{
		Target:         route.Vertex{},
		Amount:         patchTestAmt,
		FeeLimit:       lnwire.MaxMilliSatoshi,
		CltvLimit:      1000,
		FinalCLTVDelta: 40,
		MaxParts:       16,
		PaymentAddr:    fn.Some(paymentAddr),
		DestFeatures: lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadRequired,
				lnwire.PaymentAddrOptional,
				lnwire.MPPOptional,
			), lnwire.Features,
		),
	}

	var paymentHash [32]byte
	require.NoError(t, payment.SetPaymentHash(paymentHash))

	session, err := newPaymentSession(
		payment, route.Vertex{},
		func(Graph) (bandwidthHints, error) {
			return &mockBandwidthHints{}, nil
		},
		&sessionGraph{}, mc,
		PathFindingConfig{MinProbability: 0.01, Patch: patch},
	)
	require.NoError(t, err)

	return session
}

// patchTestPath is a single hop path, the smallest thing newRoute accepts.
func patchTestPath() []*unifiedEdge {
	return []*unifiedEdge{{
		policy: &models.CachedEdgePolicy{
			ToNodePubKey: func() route.Vertex {
				return route.Vertex{1}
			},
			ToNodeFeatures: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadOptional,
					lnwire.PaymentAddrOptional,
				), lnwire.Features,
			),
		},
	}}
}

// setCappedPathFinder installs a path finder that routes any amount at or
// below cap with a flat probability, and fails above it. It records every
// amount it was asked about.
func setCappedPathFinder(s *paymentSession,
	cap lnwire.MilliSatoshi) *[]lnwire.MilliSatoshi {

	return setScoredPathFinder(s, func(
		amt lnwire.MilliSatoshi) (float64, bool) {

		return 1.0, amt <= cap
	})
}

// setScoredPathFinder installs a path finder whose answer and route
// probability are supplied per amount, which is how a test plants a belief for
// the expected value ladder to price.
func setScoredPathFinder(s *paymentSession,
	score func(lnwire.MilliSatoshi) (float64, bool)) *[]lnwire.MilliSatoshi {

	probes := make([]lnwire.MilliSatoshi, 0)

	s.pathFinder = func(_ *graphParams, _ *RestrictParams,
		_ *PathFindingConfig, _, _, _ route.Vertex,
		amt lnwire.MilliSatoshi, _ float64, _ int32) ([]*unifiedEdge,
		float64, error) {

		probes = append(probes, amt)

		prob, ok := score(amt)
		if !ok {
			return nil, 0, errNoPathFound
		}

		return patchTestPath(), prob, nil
	}

	return &probes
}

// TestAdaptiveSplitDisabled asserts that with the patch off, a no-route result
// still walks the blind halving ladder and yields the same shard it always
// has.
func TestAdaptiveSplitDisabled(t *testing.T) {
	t.Parallel()

	session := newPatchTestSession(t, PatchConfig{}, &MissionControl{})
	probes := setCappedPathFinder(session, 300_000_000)

	rt, err := session.RequestRoute(
		patchTestAmt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)

	// The blind policy halves until it fits: 1e9, 5e8, 2.5e8.
	require.Equal(t, []lnwire.MilliSatoshi{
		1_000_000_000, 500_000_000, 250_000_000,
	}, *probes)
	require.EqualValues(t, 250_000_000, rt.Hops[0].AmtToForward)
}

// TestAdaptiveSplitLadder asserts the shape of the search: the rungs are the
// fixed fractions of the failing amount, in order, and none is probed twice.
func TestAdaptiveSplitLadder(t *testing.T) {
	t.Parallel()

	patch := PatchConfig{AdaptiveSplit: true}
	session := newPatchTestSession(t, patch, &MissionControl{})
	probes := setCappedPathFinder(session, 300_000_000)

	_, err := session.RequestRoute(
		patchTestAmt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)

	expected := []lnwire.MilliSatoshi{patchTestAmt}
	for _, fraction := range adaptiveSplitLadder {
		expected = append(expected, lnwire.MilliSatoshi(
			fraction*float64(patchTestAmt),
		))
	}
	require.Equal(t, expected, *probes)

	// The budget is the ladder itself, on top of the call that failed.
	require.Len(t, *probes, 1+len(adaptiveSplitLadder))
}

// TestAdaptiveSplitArgmax asserts the choice rule: among the routable rungs,
// the shard is the one maximizing fraction times route probability, not the
// largest one.
func TestAdaptiveSplitArgmax(t *testing.T) {
	t.Parallel()

	patch := PatchConfig{AdaptiveSplit: true}
	session := newPatchTestSession(t, patch, &MissionControl{})

	// A calibrated belief: the big rungs route but are barely believed,
	// the quarter rung is believed. Expected values are 0.75*0.05=0.0375,
	// 0.5*0.05=0.025, 0.375*0.05=0.019, 0.25*0.9=0.225, 0.125*0.9=0.1125,
	// so the quarter rung wins despite being far from the frontier.
	setScoredPathFinder(session, func(
		amt lnwire.MilliSatoshi) (float64, bool) {

		if amt >= patchTestAmt {
			return 0, false
		}
		if amt > patchTestAmt/4 {
			return 0.05, true
		}

		return 0.9, true
	})

	rt, err := session.RequestRoute(
		patchTestAmt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)
	require.EqualValues(
		t, patchTestAmt/4, rt.Hops[0].AmtToForward,
	)
}

// TestAdaptiveSplitFlatBelief is the degeneracy the estimator arm exists to
// test: when every routable amount is believed equally, expected value is
// maximized at the largest rung and the ladder collapses to a fixed geometric
// step.
func TestAdaptiveSplitFlatBelief(t *testing.T) {
	t.Parallel()

	patch := PatchConfig{AdaptiveSplit: true}
	session := newPatchTestSession(t, patch, &MissionControl{})
	setCappedPathFinder(session, patchTestAmt-1)

	rt, err := session.RequestRoute(
		patchTestAmt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)
	require.EqualValues(t, lnwire.MilliSatoshi(
		adaptiveSplitLadder[0]*float64(patchTestAmt),
	), rt.Hops[0].AmtToForward)
}

// TestAdaptiveSplitRungFloor asserts that rungs below the minimum shard amount
// are skipped rather than clamped, so no probe is ever spent on an amount we
// would refuse to send.
func TestAdaptiveSplitRungFloor(t *testing.T) {
	t.Parallel()

	patch := PatchConfig{AdaptiveSplit: true}
	session := newPatchTestSession(t, patch, &MissionControl{})

	// Ask for an amount whose lower rungs fall under the floor.
	const request = lnwire.MilliSatoshi(30_000_000)
	probes := setCappedPathFinder(session, request-1)

	rt, err := session.RequestRoute(
		request, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)
	require.GreaterOrEqual(t, rt.Hops[0].AmtToForward, session.minShardAmt)

	for _, probe := range (*probes)[1:] {
		require.GreaterOrEqual(t, probe, session.minShardAmt)
	}

	// 0.25 and 0.125 of 30M fall under the 10M floor, so only three rungs
	// are priced.
	require.Len(t, *probes, 1+3)
}

// TestAdaptiveSplitNoRungRoutes asserts the fallback: when belief rejects
// every rung, the payment does not abandon, it resumes the blind descent from
// just under the bottom rung and keeps halving toward the floor.
func TestAdaptiveSplitNoRungRoutes(t *testing.T) {
	t.Parallel()

	patch := PatchConfig{AdaptiveSplit: true}
	session := newPatchTestSession(t, patch, &MissionControl{})
	probes := setCappedPathFinder(session, 1)

	_, err := session.RequestRoute(
		patchTestAmt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.ErrorIs(t, err, errNoPathFound)

	bottom := lnwire.MilliSatoshi(
		adaptiveSplitLadder[len(adaptiveSplitLadder)-1] *
			float64(patchTestAmt),
	)

	// The ladder is priced once and once only, and the descent then
	// continues below its bottom rung rather than stopping there.
	require.Greater(t, len(*probes), 1+len(adaptiveSplitLadder))
	below := (*probes)[1+len(adaptiveSplitLadder):]
	require.Equal(t, bottom-1, below[0])
	for i, probe := range below {
		require.Less(t, probe, bottom)
		if i > 0 {
			require.Less(t, probe, below[i-1])
		}
	}

	// It stops at the floor, exactly as the blind policy does.
	require.GreaterOrEqual(t, below[len(below)-1], session.minShardAmt)
}

// TestAdaptiveSplitFallbackRoutes asserts that a route the fallback finds
// below the ladder is actually used, rather than being discovered and then
// discarded.
func TestAdaptiveSplitFallbackRoutes(t *testing.T) {
	t.Parallel()

	patch := PatchConfig{AdaptiveSplit: true}
	session := newPatchTestSession(t, patch, &MissionControl{})

	// Only amounts near the floor route, so every rung is rejected and the
	// shard can only come from the tail of the blind descent. Halving
	// lands near the floor rather than on it, so the cap is set at twice
	// the floor to leave the descent a rung it can take.
	cap := 2 * session.minShardAmt
	setCappedPathFinder(session, cap)

	rt, err := session.RequestRoute(
		patchTestAmt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)

	bottom := lnwire.MilliSatoshi(
		adaptiveSplitLadder[len(adaptiveSplitLadder)-1] *
			float64(patchTestAmt),
	)

	shard := rt.Hops[0].AmtToForward
	require.Less(t, shard, bottom)
	require.LessOrEqual(t, shard, cap)
	require.GreaterOrEqual(t, shard, session.minShardAmt)
}

// TestAdaptiveSplitRespectsFailAmt is the load bearing test for part A: the
// probes are ordinary path finding calls, so the amount the search settles on
// is one that mission control's recorded failure bound still permits. Nothing
// teaches the search about the bound; it falls out of asking path finding
// about a smaller amount.
func TestAdaptiveSplitRespectsFailAmt(t *testing.T) {
	t.Parallel()

	const failAmt = lnwire.MilliSatoshi(400_000_000)

	var (
		from = route.Vertex{10}
		to   = route.Vertex{11}
	)

	mc := newPatchTestMC(t, PatchConfig{})

	// Plant a bound: the second pair of this route could not carry
	// failAmt, reported by its upstream node as a temporary channel
	// failure, which is the ordinary way a liquidity bound is learned.
	rt := &route.Route{
		SourcePubKey: route.Vertex{9},
		TotalAmount:  failAmt,
		Hops: []*route.Hop{
			{PubKeyBytes: from, AmtToForward: failAmt},
			{PubKeyBytes: to, AmtToForward: failAmt},
		},
	}
	failIdx := 1
	_, err := mc.ReportPaymentFail(
		0, rt, &failIdx, lnwire.NewTemporaryChannelFailure(nil),
	)
	require.NoError(t, err)
	require.EqualValues(
		t, failAmt, mc.GetPairHistorySnapshot(from, to).FailAmt,
	)

	patch := PatchConfig{AdaptiveSplit: true}
	session := newPatchTestSession(t, patch, mc)

	// A miniature of path finding: the only edge to the target is the pair
	// we just planted a bound on, and it is only usable while the
	// estimator still gives it a chance at the amount asked for.
	probes := make([]lnwire.MilliSatoshi, 0)
	session.pathFinder = func(_ *graphParams, r *RestrictParams,
		cfg *PathFindingConfig, _, _, _ route.Vertex,
		amt lnwire.MilliSatoshi, _ float64, _ int32) ([]*unifiedEdge,
		float64, error) {

		probes = append(probes, amt)

		prob := r.ProbabilitySource(from, to, amt, 0)
		if prob < cfg.MinProbability {
			return nil, 0, errNoPathFound
		}

		return patchTestPath(), prob, nil
	}

	found, err := session.RequestRoute(
		patchTestAmt, lnwire.MaxMilliSatoshi, 0, 0, nil,
	)
	require.NoError(t, err)

	// The search must land strictly under the planted bound: the estimator
	// zeroes the pair at or above failAmt, and passes it below.
	shard := found.Hops[0].AmtToForward
	require.Less(t, shard, failAmt)
	require.GreaterOrEqual(t, shard, session.minShardAmt)

	// The bound is what stopped it, not the budget: the rungs above the
	// bound were priced and refused, and a rung below it was taken.
	require.Len(t, probes, 1+len(adaptiveSplitLadder))
}

// newPatchTestMC builds a real mission control instance on a throwaway db.
func newPatchTestMC(t *testing.T, patch PatchConfig) *MissionControl {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "*.db")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	db, err := kvdb.Open(
		kvdb.BoltBackendName, file.Name(), true,
		kvdb.DefaultDBTimeout, false,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	estimator, err := NewAprioriEstimator(AprioriConfig{
		PenaltyHalfLife:       testPenaltyHalfLife,
		AprioriHopProbability: testAprioriHopProbability,
		AprioriWeight:         testAprioriWeight,
		CapacityFraction:      testCapacityFraction,
	})
	require.NoError(t, err)

	mcCfg := &MissionControlConfig{Estimator: estimator, Patch: patch}
	controller, err := NewMissionController(db, route.Vertex{}, mcCfg)
	require.NoError(t, err)

	mc, err := controller.GetNamespacedStore(
		DefaultMissionControlNamespace,
	)
	require.NoError(t, err)

	return mc
}

// patchTestRoute builds an mcRoute of n hops away from a fixed source, every
// hop forwarding the same amount.
func patchTestRoute(n int, amt lnwire.MilliSatoshi) *mcRoute {
	hops := make([]*route.Hop, n)
	for i := range hops {
		hops[i] = &route.Hop{
			ChannelID:    uint64(i + 1),
			PubKeyBytes:  route.Vertex{byte(i + 1)},
			AmtToForward: amt,
		}
	}

	return extractMCRoute(&route.Route{
		SourcePubKey: route.Vertex{},
		TotalAmount:  amt,
		Hops:         hops,
	})
}

// TestSoftUnknownDisabled asserts that with the patch off an unattributable
// failure still blacklists every pair of the route in both directions.
func TestSoftUnknownDisabled(t *testing.T) {
	t.Parallel()

	const amt = lnwire.MilliSatoshi(100_000)

	rt := patchTestRoute(3, amt)
	i := interpretResult(rt, fn.Some(newPaymentFailure(nil, nil)), nil)

	// Three hops, both directions, all at amount zero.
	require.Len(t, i.pairResults, 6)
	for _, result := range i.pairResults {
		require.False(t, result.success)
		require.EqualValues(t, 0, result.amt)
	}
}

// TestSoftUnknownSinglePair asserts the semantics of part B: exactly one pair
// is penalized, it is the lowest probability hop of the route, the penalty is
// recorded at the amount that hop was asked to forward, and the reverse
// direction is left alone.
func TestSoftUnknownSinglePair(t *testing.T) {
	t.Parallel()

	const amt = lnwire.MilliSatoshi(100_000)

	rt := patchTestRoute(3, amt)

	// Make the middle hop the least promising one.
	weakest := NewDirectedNodePair(
		rt.hops.Val[0].pubKeyBytes.Val, rt.hops.Val[1].pubKeyBytes.Val,
	)
	probability := func(from, to route.Vertex,
		_ lnwire.MilliSatoshi) float64 {

		if NewDirectedNodePair(from, to) == weakest {
			return 0.1
		}

		return 0.9
	}

	i := interpretResult(
		rt, fn.Some(newPaymentFailure(nil, nil)), probability,
	)

	require.Len(t, i.pairResults, 1)

	result, ok := i.pairResults[weakest]
	require.True(t, ok, "weakest pair not penalized")
	require.False(t, result.success)
	require.EqualValues(t, amt, result.amt)

	// The reverse direction carries no evidence and must not be touched.
	_, ok = i.pairResults[weakest.Reverse()]
	require.False(t, ok)

	// A single hop route keeps the existing node level treatment.
	single := interpretResult(
		patchTestRoute(1, amt),
		fn.Some(newPaymentFailure(nil, nil)), probability,
	)
	require.NotNil(t, single.nodeFailure)
	require.NotNil(t, single.finalFailureReason)
}

// TestSoftUnknownTieBreak asserts that when the estimator cannot separate the
// hops, the penalty goes to the hop furthest from us, the one we know least
// about.
func TestSoftUnknownTieBreak(t *testing.T) {
	t.Parallel()

	const amt = lnwire.MilliSatoshi(100_000)

	rt := patchTestRoute(3, amt)
	flat := func(_, _ route.Vertex, _ lnwire.MilliSatoshi) float64 {
		return 0.5
	}

	i := interpretResult(rt, fn.Some(newPaymentFailure(nil, nil)), flat)

	last := NewDirectedNodePair(
		rt.hops.Val[1].pubKeyBytes.Val, rt.hops.Val[2].pubKeyBytes.Val,
	)
	require.Len(t, i.pairResults, 1)
	require.Contains(t, i.pairResults, last)
}

// TestSoftUnknownEndToEnd drives the policy through a real mission control, to
// confirm the config knob reaches the interpretation and that the recorded
// entry is a bound a smaller retry can route around rather than a blacklist.
func TestSoftUnknownEndToEnd(t *testing.T) {
	t.Parallel()

	const amt = lnwire.MilliSatoshi(100_000)

	mc := newPatchTestMC(t, PatchConfig{SoftUnknown: true})

	rt := &route.Route{
		SourcePubKey: route.Vertex{},
		TotalAmount:  amt,
		Hops: []*route.Hop{
			{PubKeyBytes: route.Vertex{1}, AmtToForward: amt},
			{PubKeyBytes: route.Vertex{2}, AmtToForward: amt},
			{PubKeyBytes: route.Vertex{3}, AmtToForward: amt},
		},
	}

	// A nil failure source and message is how an unreadable onion error
	// arrives.
	_, err := mc.ReportPaymentFail(0, rt, nil, nil)
	require.NoError(t, err)

	snapshot := mc.GetHistorySnapshot()
	require.Len(t, snapshot.Pairs, 1)

	// The recorded failure amount is the attempt amount, not zero, which
	// is what makes it a bound.
	require.EqualValues(t, amt, snapshot.Pairs[0].TimedPairResult.FailAmt)
}
