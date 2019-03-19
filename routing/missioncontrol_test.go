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

	mc.reportEdgeFailure(&testEdge)
	expectP(0)

	now = testTime.Add(time.Minute * 15)
	expectP(0.2)

	now = testTime.Add(time.Minute * 60)
	expectP(0.8)
}
