package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// TestMissionControl tests mission control probability estimation.
func TestMissionControl(t *testing.T) {
	now := testTime

	mc := NewMissionControl(
		nil, nil, nil, &MissionControlConfig{
			PenaltyHalfLife:       30 * time.Minute,
			AprioriHopProbability: 0.8,
		},
	)
	mc.now = func() time.Time { return now }

	testTime := time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	testNode := route.Vertex{}
	testEdge := edge{
		channel: 123,
	}

	expectP := func(amt lnwire.MilliSatoshi, expected float64) {
		t.Helper()

		p := mc.getEdgeProbability(
			testNode, EdgeLocator{ChannelID: testEdge.channel},
			amt,
		)
		if p != expected {
			t.Fatalf("unexpected probability %v", p)
		}
	}

	// Initial probability is expected to be 1.
	expectP(1000, 0.8)

	// Expect probability to be zero after reporting the edge as failed.
	mc.reportEdgeFailure(testEdge, 1000)
	expectP(1000, 0)

	// As we reported with a min penalization amt, a lower amt than reported
	// should be unaffected.
	expectP(500, 0.8)

	// Edge decay started.
	now = testTime.Add(30 * time.Minute)
	expectP(1000, 0.4)

	// Edge fails again, this time without a min penalization amt. The edge
	// should be penalized regardless of amount.
	mc.reportEdgeFailure(testEdge, 0)
	expectP(1000, 0)
	expectP(500, 0)

	// Edge decay started.
	now = testTime.Add(60 * time.Minute)
	expectP(1000, 0.4)

	// A node level failure should bring probability of every channel back
	// to zero.
	mc.reportVertexFailure(testNode)
	expectP(1000, 0)

	// Check whether history snapshot looks sane.
	history := mc.GetHistorySnapshot()
	if len(history.Nodes) != 1 {
		t.Fatal("unexpected number of nodes")
	}

	if len(history.Nodes[0].Channels) != 1 {
		t.Fatal("unexpected number of channels")
	}
}
