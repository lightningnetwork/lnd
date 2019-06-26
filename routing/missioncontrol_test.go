package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	mcTestNode = route.Vertex{}
	mcTestEdge = EdgeLocator{
		ChannelID: 123,
	}
	mcTestTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)
)

type mcTestContext struct {
	t   *testing.T
	mc  *MissionControl
	now time.Time
}

func createMcTestContext(t *testing.T) *mcTestContext {
	ctx := &mcTestContext{
		t:   t,
		now: mcTestTime,
	}

	mc := NewMissionControl(
		&MissionControlConfig{
			PenaltyHalfLife:       30 * time.Minute,
			AprioriHopProbability: 0.8,
		},
	)

	mc.now = func() time.Time { return ctx.now }
	ctx.mc = mc

	return ctx
}

// Assert that mission control returns a probability for an edge.
func (ctx *mcTestContext) expectP(amt lnwire.MilliSatoshi,
	expected float64) {

	ctx.t.Helper()

	p := ctx.mc.GetEdgeProbability(mcTestNode, mcTestEdge, amt)
	if p != expected {
		ctx.t.Fatalf("unexpected probability %v", p)
	}
}

// TestMissionControl tests mission control probability estimation.
func TestMissionControl(t *testing.T) {
	ctx := createMcTestContext(t)

	ctx.now = testTime

	testTime := time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	testNode := route.Vertex{}
	testEdge := edge{
		channel: 123,
	}

	// Initial probability is expected to be 1.
	ctx.expectP(1000, 0.8)

	// Expect probability to be zero after reporting the edge as failed.
	ctx.mc.ReportEdgeFailure(testEdge, 1000)
	ctx.expectP(1000, 0)

	// As we reported with a min penalization amt, a lower amt than reported
	// should be unaffected.
	ctx.expectP(500, 0.8)

	// Edge decay started.
	ctx.now = testTime.Add(30 * time.Minute)
	ctx.expectP(1000, 0.4)

	// Edge fails again, this time without a min penalization amt. The edge
	// should be penalized regardless of amount.
	ctx.mc.ReportEdgeFailure(testEdge, 0)
	ctx.expectP(1000, 0)
	ctx.expectP(500, 0)

	// Edge decay started.
	ctx.now = testTime.Add(60 * time.Minute)
	ctx.expectP(1000, 0.4)

	// A node level failure should bring probability of every channel back
	// to zero.
	ctx.mc.ReportVertexFailure(testNode)
	ctx.expectP(1000, 0)

	// Check whether history snapshot looks sane.
	history := ctx.mc.GetHistorySnapshot()
	if len(history.Nodes) != 1 {
		t.Fatal("unexpected number of nodes")
	}

	if len(history.Nodes[0].Channels) != 1 {
		t.Fatal("unexpected number of channels")
	}
}

// TestMissionControlChannelUpdate tests that the first channel update is not
// penalizing the channel yet.
func TestMissionControlChannelUpdate(t *testing.T) {
	ctx := createMcTestContext(t)

	testEdge := edge{
		channel: 123,
	}

	// Report a policy related failure. Because it is the first, we don't
	// expect a penalty.
	ctx.mc.ReportEdgePolicyFailure(testEdge)

	ctx.expectP(0, 0.8)

	// Report another failure for the same channel. We expect it to be
	// pruned.
	ctx.mc.ReportEdgePolicyFailure(testEdge)

	ctx.expectP(0, 0)
}
