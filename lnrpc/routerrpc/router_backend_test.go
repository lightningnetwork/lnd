package routerrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"

	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	destKey       = "0286098b97bc843372b4426d4b276cea9aa2f48f0428d6f5b66ae101befc14f8b4"
	ignoreNodeKey = "02f274f48f3c0d590449a6776e3ce8825076ac376e470e992246eebc565ef8bb2a"

	testMissionControlProb = 0.5
)

var (
	sourceKey = route.Vertex{1, 2, 3}
)

// TestQueryRoutes asserts that query routes rpc parameters are properly parsed
// and passed onto path finding.
func TestQueryRoutes(t *testing.T) {
	t.Run("no mission control", func(t *testing.T) {
		testQueryRoutes(t, false)
	})
	t.Run("with mission control", func(t *testing.T) {
		testQueryRoutes(t, true)
	})
}

func testQueryRoutes(t *testing.T, useMissionControl bool) {
	ignoreNodeBytes, err := hex.DecodeString(ignoreNodeKey)
	if err != nil {
		t.Fatal(err)
	}

	var ignoreNodeVertex route.Vertex
	copy(ignoreNodeVertex[:], ignoreNodeBytes)

	destNodeBytes, err := hex.DecodeString(destKey)
	if err != nil {
		t.Fatal(err)
	}

	ignoredEdge := routing.EdgeLocator{
		ChannelID: 555,
		Direction: 1,
	}

	request := &lnrpc.QueryRoutesRequest{
		PubKey:         destKey,
		Amt:            100000,
		FinalCltvDelta: 100,
		FeeLimit: &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_Fixed{
				Fixed: 250,
			},
		},
		IgnoredNodes: [][]byte{ignoreNodeBytes},
		IgnoredEdges: []*lnrpc.EdgeLocator{{
			ChannelId:        555,
			DirectionReverse: true,
		}},
		UseMissionControl: useMissionControl,
	}

	findRoute := func(source, target route.Vertex,
		amt lnwire.MilliSatoshi, restrictions *routing.RestrictParams,
		finalExpiry ...uint16) (*route.Route, error) {

		if int64(amt) != request.Amt*1000 {
			t.Fatal("unexpected amount")
		}

		if source != sourceKey {
			t.Fatal("unexpected source key")
		}

		if !bytes.Equal(target[:], destNodeBytes) {
			t.Fatal("unexpected target key")
		}

		if restrictions.FeeLimit != 250*1000 {
			t.Fatal("unexpected fee limit")
		}

		if restrictions.ProbabilitySource(route.Vertex{},
			ignoredEdge, 0,
		) != 0 {
			t.Fatal("expecting 0% probability for ignored edge")
		}

		if restrictions.ProbabilitySource(ignoreNodeVertex,
			routing.EdgeLocator{}, 0,
		) != 0 {
			t.Fatal("expecting 0% probability for ignored node")
		}

		expectedProb := 1.0
		if useMissionControl {
			expectedProb = testMissionControlProb
		}
		if restrictions.ProbabilitySource(route.Vertex{},
			routing.EdgeLocator{}, 0,
		) != expectedProb {
			t.Fatal("expecting 100% probability")
		}

		hops := []*route.Hop{{}}
		return route.NewRouteFromHops(amt, 144, source, hops)
	}

	backend := &RouterBackend{
		MaxPaymentMSat: lnwire.NewMSatFromSatoshis(1000000),
		FindRoute:      findRoute,
		SelfNode:       route.Vertex{1, 2, 3},
		FetchChannelCapacity: func(chanID uint64) (
			btcutil.Amount, error) {

			return 1, nil
		},
		MissionControl: &mockMissionControl{},
	}

	resp, err := backend.QueryRoutes(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Routes) != 1 {
		t.Fatal("expected a single route response")
	}
}

type mockMissionControl struct {
}

func (m *mockMissionControl) GetEdgeProbability(fromNode route.Vertex,
	edge routing.EdgeLocator, amt lnwire.MilliSatoshi) float64 {
	return testMissionControlProb
}

func (m *mockMissionControl) ResetHistory() {}

func (m *mockMissionControl) GetHistorySnapshot() *routing.MissionControlSnapshot {
	return nil
}
