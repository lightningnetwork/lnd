package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/routing/route"
)

// TestMissionControl tests mission control probability estimation.
func TestMissionControl(t *testing.T) {
	now := testTime

	mc := NewMissionControl(nil, nil, nil)
	mc.now = func() time.Time { return now }
	mc.hardPruneDuration = 5 * time.Minute
	mc.penaltyHalfLife = 30 * time.Minute

	testNode := route.Vertex{}
	testEdge := EdgeLocator{
		ChannelID: 123,
	}

	expectP := func(expected float64) {
		t.Helper()

		p := mc.getEdgeProbability(testNode, testEdge)
		if p != expected {
			t.Fatalf("unexpected probability %v", p)
		}
	}

	// Initial probability is expected to be 1.
	expectP(1)

	// Expect probability to be zero after reporting the edge as failed.
	mc.reportEdgeFailure(edge{channel: testEdge.ChannelID})
	expectP(0)

	// Still in the hard prune window.
	now = testTime.Add(5 * time.Minute)
	expectP(0)

	// Edge decay started.
	now = testTime.Add(35 * time.Minute)
	expectP(0.5)

	// Edge fails again
	mc.reportEdgeFailure(edge{channel: testEdge.ChannelID})
	expectP(0)

	// Edge decay started.
	now = testTime.Add(70 * time.Minute)
	expectP(0.5)
}
