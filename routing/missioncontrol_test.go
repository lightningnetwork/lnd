package routing

import (
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	mcTestRoute = &route.Route{
		SourcePubKey: route.Vertex{10},
		Hops: []*route.Hop{
			{
				ChannelID:    1,
				PubKeyBytes:  route.Vertex{11},
				AmtToForward: 1000,
			},
			{
				ChannelID:   2,
				PubKeyBytes: route.Vertex{12},
			},
		},
	}

	mcTestTime  = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)
	mcTestNode1 = mcTestRoute.Hops[0].PubKeyBytes
	mcTestNode2 = mcTestRoute.Hops[1].PubKeyBytes
)

type mcTestContext struct {
	t   *testing.T
	mc  *MissionControl
	now time.Time

	db     *bbolt.DB
	dbPath string

	pid uint64
}

func createMcTestContext(t *testing.T) *mcTestContext {
	ctx := &mcTestContext{
		t:   t,
		now: mcTestTime,
	}

	file, err := ioutil.TempFile("", "*.db")
	if err != nil {
		t.Fatal(err)
	}

	ctx.dbPath = file.Name()

	ctx.db, err = bbolt.Open(ctx.dbPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx.restartMc()

	return ctx
}

// restartMc creates a new instances of mission control on the same database.
func (ctx *mcTestContext) restartMc() {
	mc, err := NewMissionControl(
		ctx.db,
		&MissionControlConfig{
			PenaltyHalfLife:       30 * time.Minute,
			AprioriHopProbability: 0.8,
		},
	)
	if err != nil {
		ctx.t.Fatal(err)
	}

	mc.now = func() time.Time { return ctx.now }
	ctx.mc = mc
}

// cleanup closes the database and removes the temp file.
func (ctx *mcTestContext) cleanup() {
	ctx.db.Close()
	os.Remove(ctx.dbPath)
}

// Assert that mission control returns a probability for an edge.
func (ctx *mcTestContext) expectP(amt lnwire.MilliSatoshi, expected float64) {

	ctx.t.Helper()

	p := ctx.mc.GetEdgeProbability(mcTestNode1, mcTestNode2, amt)
	if p != expected {
		ctx.t.Fatalf("expected probability %v but got %v", expected, p)
	}
}

// reportFailure reports a failure by using a test route.
func (ctx *mcTestContext) reportFailure(t time.Time,
	amt lnwire.MilliSatoshi, failure lnwire.FailureMessage) {

	mcTestRoute.Hops[0].AmtToForward = amt

	errorSourceIdx := 1
	ctx.mc.ReportPaymentFail(
		ctx.pid, mcTestRoute, &errorSourceIdx, failure,
	)
}

// TestMissionControl tests mission control probability estimation.
func TestMissionControl(t *testing.T) {
	ctx := createMcTestContext(t)
	defer ctx.cleanup()

	ctx.now = testTime

	testTime := time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	// Initial probability is expected to be 1.
	ctx.expectP(1000, 0.8)

	// Expect probability to be zero after reporting the edge as failed.
	ctx.reportFailure(
		testTime, 1000,
		lnwire.NewTemporaryChannelFailure(nil),
	)
	ctx.expectP(1000, 0)

	// As we reported with a min penalization amt, a lower amt than reported
	// should be unaffected.
	ctx.expectP(500, 0.8)

	// Edge decay started.
	ctx.now = testTime.Add(30 * time.Minute)
	ctx.expectP(1000, 0.4)

	// Edge fails again, this time without a min penalization amt. The edge
	// should be penalized regardless of amount.
	ctx.reportFailure(
		ctx.now, 0,
		lnwire.NewTemporaryChannelFailure(nil),
	)
	ctx.expectP(1000, 0)
	ctx.expectP(500, 0)

	// Edge decay started.
	ctx.now = testTime.Add(60 * time.Minute)
	ctx.expectP(1000, 0.4)

	// Restart mission control to test persistence.
	ctx.restartMc()
	ctx.expectP(1000, 0.4)

	// A node level failure should bring probability of every channel back
	// to zero.
	ctx.reportFailure(
		ctx.now, 0,
		lnwire.NewExpiryTooSoon(lnwire.ChannelUpdate{}),
	)
	ctx.expectP(1000, 0)

	// Check whether history snapshot looks sane.
	history := ctx.mc.GetHistorySnapshot()
	if len(history.Nodes) != 1 {
		t.Fatal("unexpected number of nodes")
	}

	if len(history.Pairs) != 1 {
		t.Fatal("unexpected number of channels")
	}
}

// TestMissionControlChannelUpdate tests that the first channel update is not
// penalizing the channel yet.
func TestMissionControlChannelUpdate(t *testing.T) {
	ctx := createMcTestContext(t)

	// Report a policy related failure. Because it is the first, we don't
	// expect a penalty.
	ctx.reportFailure(
		ctx.now, 0,
		lnwire.NewFeeInsufficient(0, lnwire.ChannelUpdate{}),
	)
	ctx.expectP(0, 0.8)

	// Report another failure for the same channel. We expect it to be
	// pruned.
	ctx.reportFailure(
		ctx.now, 0,
		lnwire.NewFeeInsufficient(0, lnwire.ChannelUpdate{}),
	)
	ctx.expectP(0, 0)
}
