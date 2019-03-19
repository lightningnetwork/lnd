package routing

import (
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
)

func TestMissionControl(t *testing.T) {
	now := testTime

	mc := newMissionControl(nil, nil, nil)
	mc.now = func() time.Time { return now }
	mc.hardPruneDuration = 5 * time.Minute
	mc.edgeDecay = time.Hour
	mc.vertexDecay = 2 * time.Hour

	testNode := Vertex{}
	testEdge := EdgeLocator{
		ChannelID: 123,
	}

	amt := lnwire.NewMSatFromSatoshis(1000)
	capacity := btcutil.Amount(5000)

	expectP := func(expected float64) {
		p := mc.getEdgeProbability(testNode, amt, testEdge, capacity)
		if p != expected {
			t.Fatalf("unexpected probability %v", p)
		}
	}

	expectP(0.8)

	// Expect probability to be zero after reporting the edge as failed.
	mc.reportEdgeFailure(&testEdge)
	expectP(0)

	// Still in the hard prune window.
	now = testTime.Add(5 * time.Minute)
	expectP(0)

	// Edge decay started.
	now = testTime.Add(20 * time.Minute)
	expectP(0.2)

	// Edge fully decayed and probability back up to the a priori value.
	now = testTime.Add(65 * time.Minute)
	expectP(0.8)
}
