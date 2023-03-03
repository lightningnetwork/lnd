package routing

import (
	"os"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	mcTestRoute = &route.Route{
		SourcePubKey: mcTestSelf,
		Hops: []*route.Hop{
			{
				ChannelID:     1,
				PubKeyBytes:   route.Vertex{11},
				AmtToForward:  1000,
				LegacyPayload: true,
			},
			{
				ChannelID:     2,
				PubKeyBytes:   route.Vertex{12},
				LegacyPayload: true,
			},
		},
	}

	mcTestTime  = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)
	mcTestSelf  = route.Vertex{10}
	mcTestNode1 = mcTestRoute.Hops[0].PubKeyBytes
	mcTestNode2 = mcTestRoute.Hops[1].PubKeyBytes

	testPenaltyHalfLife       = 30 * time.Minute
	testAprioriHopProbability = 0.9
	testAprioriWeight         = 0.5
)

type mcTestContext struct {
	t   *testing.T
	mc  *MissionControl
	now time.Time

	db     kvdb.Backend
	dbPath string

	pid uint64
}

func createMcTestContext(t *testing.T) *mcTestContext {
	ctx := &mcTestContext{
		t:   t,
		now: mcTestTime,
	}

	file, err := os.CreateTemp("", "*.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, file.Close())
		require.NoError(t, os.Remove(file.Name()))
	})

	ctx.dbPath = file.Name()

	ctx.db, err = kvdb.Open(
		kvdb.BoltBackendName, ctx.dbPath, true, kvdb.DefaultDBTimeout,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, ctx.db.Close())
	})

	ctx.restartMc()

	return ctx
}

// restartMc creates a new instances of mission control on the same database.
func (ctx *mcTestContext) restartMc() {
	// Since we don't run a timer to store results in unit tests, we store
	// them here before fetching back everything in NewMissionControl.
	if ctx.mc != nil {
		require.NoError(ctx.t, ctx.mc.store.storeResults())
	}

	aCfg := AprioriConfig{
		PenaltyHalfLife:       testPenaltyHalfLife,
		AprioriHopProbability: testAprioriHopProbability,
		AprioriWeight:         testAprioriWeight,
		CapacityFraction:      testCapacityFraction,
	}
	estimator, err := NewAprioriEstimator(aCfg)
	require.NoError(ctx.t, err)

	mc, err := NewMissionControl(
		ctx.db, mcTestSelf,
		&MissionControlConfig{Estimator: estimator},
	)
	if err != nil {
		ctx.t.Fatal(err)
	}

	mc.now = func() time.Time { return ctx.now }
	ctx.mc = mc
}

// Assert that mission control returns a probability for an edge.
func (ctx *mcTestContext) expectP(amt lnwire.MilliSatoshi, expected float64) {
	ctx.t.Helper()

	p := ctx.mc.GetProbability(mcTestNode1, mcTestNode2, amt, testCapacity)

	// We relax the accuracy for the probability check because of the
	// capacity cutoff factor.
	require.InDelta(
		ctx.t, expected, p, 0.001, "probability does not match",
	)
}

// reportFailure reports a failure by using a test route.
func (ctx *mcTestContext) reportFailure(amt lnwire.MilliSatoshi,
	failure lnwire.FailureMessage) {

	mcTestRoute.Hops[0].AmtToForward = amt

	errorSourceIdx := 1
	ctx.mc.ReportPaymentFail(
		ctx.pid, mcTestRoute, &errorSourceIdx, failure,
	)
}

// reportSuccess reports a success by using a test route.
func (ctx *mcTestContext) reportSuccess() {
	err := ctx.mc.ReportPaymentSuccess(ctx.pid, mcTestRoute)
	if err != nil {
		ctx.t.Fatal(err)
	}

	ctx.pid++
}

// TestMissionControl tests mission control probability estimation.
func TestMissionControl(t *testing.T) {
	ctx := createMcTestContext(t)

	ctx.now = testTime

	testTime := time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	// For local channels, we expect a higher probability than our apriori
	// test probability.
	selfP := ctx.mc.GetProbability(
		mcTestSelf, mcTestNode1, 100, testCapacity,
	)
	if selfP != prevSuccessProbability {
		t.Fatalf("expected prev success prob for untried local chans")
	}

	// Initial probability is expected to be the a priori.
	ctx.expectP(1000, testAprioriHopProbability)

	// Expect probability to be zero after reporting the edge as failed.
	ctx.reportFailure(1000, lnwire.NewTemporaryChannelFailure(nil))
	ctx.expectP(1000, 0)

	// As we reported with a min penalization amt, a lower amt than reported
	// should return the node probability, which is the a priori
	// probability.
	ctx.expectP(500, testAprioriHopProbability)

	// Edge decay started. The node probability weighted average should now
	// have shifted from 1:1 to 1:0.5 -> 60%. The connection probability is
	// half way through the recovery, so we expect 30% here.
	ctx.now = testTime.Add(30 * time.Minute)
	ctx.expectP(1000, 0.3)

	// Edge fails again, this time without a min penalization amt. The edge
	// should be penalized regardless of amount.
	ctx.reportFailure(0, lnwire.NewTemporaryChannelFailure(nil))
	ctx.expectP(1000, 0)
	ctx.expectP(500, 0)

	// Edge decay started.
	ctx.now = testTime.Add(60 * time.Minute)
	ctx.expectP(1000, 0.3)

	// Restart mission control to test persistence.
	ctx.restartMc()
	ctx.expectP(1000, 0.3)

	// A node level failure should bring probability of all known channels
	// back to zero.
	ctx.reportFailure(0, lnwire.NewExpiryTooSoon(lnwire.ChannelUpdate{}))
	ctx.expectP(1000, 0)

	// Check whether history snapshot looks sane.
	history := ctx.mc.GetHistorySnapshot()

	if len(history.Pairs) != 4 {
		t.Fatalf("expected 4 pairs, but got %v", len(history.Pairs))
	}

	// Test reporting a success.
	ctx.reportSuccess()
}

// TestMissionControlChannelUpdate tests that the first channel update is not
// penalizing the channel yet.
func TestMissionControlChannelUpdate(t *testing.T) {
	ctx := createMcTestContext(t)

	// Report a policy related failure. Because it is the first, we don't
	// expect a penalty.
	ctx.reportFailure(
		0, lnwire.NewFeeInsufficient(0, lnwire.ChannelUpdate{}),
	)
	ctx.expectP(100, testAprioriHopProbability)

	// Report another failure for the same channel. We expect it to be
	// pruned.
	ctx.reportFailure(
		0, lnwire.NewFeeInsufficient(0, lnwire.ChannelUpdate{}),
	)
	ctx.expectP(100, 0)
}
