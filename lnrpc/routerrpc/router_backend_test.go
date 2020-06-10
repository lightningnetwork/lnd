package routerrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"

	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	destKey       = "0286098b97bc843372b4426d4b276cea9aa2f48f0428d6f5b66ae101befc14f8b4"
	ignoreNodeKey = "02f274f48f3c0d590449a6776e3ce8825076ac376e470e992246eebc565ef8bb2a"
	hintNodeKey   = "0274e7fb33eafd74fe1acb6db7680bb4aa78e9c839a6e954e38abfad680f645ef7"

	testMissionControlProb = 0.5
)

var (
	sourceKey = route.Vertex{1, 2, 3}

	node1 = route.Vertex{10}

	node2 = route.Vertex{11}
)

// TestQueryRoutes asserts that query routes rpc parameters are properly parsed
// and passed onto path finding.
func TestQueryRoutes(t *testing.T) {
	t.Run("no mission control", func(t *testing.T) {
		testQueryRoutes(t, false, false)
	})
	t.Run("no mission control and msat", func(t *testing.T) {
		testQueryRoutes(t, false, true)
	})
	t.Run("with mission control", func(t *testing.T) {
		testQueryRoutes(t, true, false)
	})
}

func testQueryRoutes(t *testing.T, useMissionControl bool, useMsat bool) {
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

	var (
		lastHop      = route.Vertex{64}
		outgoingChan = uint64(383322)
	)

	hintNode, err := route.NewVertexFromStr(hintNodeKey)
	if err != nil {
		t.Fatal(err)
	}

	rpcRouteHints := []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{
				{
					ChanId: 38484,
					NodeId: hintNodeKey,
				},
			},
		},
	}

	request := &lnrpc.QueryRoutesRequest{
		PubKey:         destKey,
		FinalCltvDelta: 100,
		IgnoredNodes:   [][]byte{ignoreNodeBytes},
		IgnoredEdges: []*lnrpc.EdgeLocator{{
			ChannelId:        555,
			DirectionReverse: true,
		}},
		IgnoredPairs: []*lnrpc.NodePair{{
			From: node1[:],
			To:   node2[:],
		}},
		UseMissionControl: useMissionControl,
		LastHopPubkey:     lastHop[:],
		OutgoingChanId:    outgoingChan,
		DestFeatures:      []lnrpc.FeatureBit{lnrpc.FeatureBit_MPP_OPT},
		RouteHints:        rpcRouteHints,
	}

	amtSat := int64(100000)
	if useMsat {
		request.AmtMsat = amtSat * 1000
		request.FeeLimit = &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_FixedMsat{
				FixedMsat: 250000,
			},
		}
	} else {
		request.Amt = amtSat
		request.FeeLimit = &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_Fixed{
				Fixed: 250,
			},
		}
	}

	findRoute := func(source, target route.Vertex,
		amt lnwire.MilliSatoshi, restrictions *routing.RestrictParams,
		_ record.CustomSet,
		routeHints map[route.Vertex][]*channeldb.ChannelEdgePolicy,
		finalExpiry uint16) (*route.Route, error) {

		if int64(amt) != amtSat*1000 {
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

		if restrictions.ProbabilitySource(route.Vertex{2},
			route.Vertex{1}, 0,
		) != 0 {
			t.Fatal("expecting 0% probability for ignored edge")
		}

		if restrictions.ProbabilitySource(ignoreNodeVertex,
			route.Vertex{6}, 0,
		) != 0 {
			t.Fatal("expecting 0% probability for ignored node")
		}

		if restrictions.ProbabilitySource(node1, node2, 0) != 0 {
			t.Fatal("expecting 0% probability for ignored pair")
		}

		if *restrictions.LastHop != lastHop {
			t.Fatal("unexpected last hop")
		}

		if restrictions.OutgoingChannelIDs[0] != outgoingChan {
			t.Fatal("unexpected outgoing channel id")
		}

		if !restrictions.DestFeatures.HasFeature(lnwire.MPPOptional) {
			t.Fatal("unexpected dest features")
		}

		if _, ok := routeHints[hintNode]; !ok {
			t.Fatal("expected route hint")
		}

		expectedProb := 1.0
		if useMissionControl {
			expectedProb = testMissionControlProb
		}
		if restrictions.ProbabilitySource(route.Vertex{4},
			route.Vertex{5}, 0,
		) != expectedProb {
			t.Fatal("expecting 100% probability")
		}

		hops := []*route.Hop{{}}
		return route.NewRouteFromHops(amt, 144, source, hops)
	}

	backend := &RouterBackend{
		FindRoute: findRoute,
		SelfNode:  route.Vertex{1, 2, 3},
		FetchChannelCapacity: func(chanID uint64) (
			btcutil.Amount, error) {

			return 1, nil
		},
		MissionControl: &mockMissionControl{},
		FetchChannelEndpoints: func(chanID uint64) (route.Vertex,
			route.Vertex, error) {

			if chanID != 555 {
				t.Fatal("expected endpoints to be fetched for "+
					"channel 555, but got %v instead",
					chanID)
			}
			return route.Vertex{1}, route.Vertex{2}, nil
		},
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

func (m *mockMissionControl) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi) float64 {

	return testMissionControlProb
}

func (m *mockMissionControl) ResetHistory() error {
	return nil
}

func (m *mockMissionControl) GetHistorySnapshot() *routing.MissionControlSnapshot {
	return nil
}

func (m *mockMissionControl) GetPairHistorySnapshot(fromNode,
	toNode route.Vertex) routing.TimedPairResult {

	return routing.TimedPairResult{}
}

type mppOutcome byte

const (
	valid mppOutcome = iota
	invalid
	nompp
)

type unmarshalMPPTest struct {
	name    string
	mpp     *lnrpc.MPPRecord
	outcome mppOutcome
}

// TestUnmarshalMPP checks both positive and negative cases of UnmarshalMPP to
// assert that an MPP record is only returned when both fields are properly
// specified. It also asserts that zero-values for both inputs is also valid,
// but returns a nil record.
func TestUnmarshalMPP(t *testing.T) {
	tests := []unmarshalMPPTest{
		{
			name:    "nil record",
			mpp:     nil,
			outcome: nompp,
		},
		{
			name: "invalid total or addr",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  nil,
				TotalAmtMsat: 0,
			},
			outcome: invalid,
		},
		{
			name: "valid total only",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  nil,
				TotalAmtMsat: 8,
			},
			outcome: invalid,
		},
		{
			name: "valid addr only",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  bytes.Repeat([]byte{0x02}, 32),
				TotalAmtMsat: 0,
			},
			outcome: invalid,
		},
		{
			name: "valid total and invalid addr",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  []byte{0x02},
				TotalAmtMsat: 8,
			},
			outcome: invalid,
		},
		{
			name: "valid total and valid addr",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  bytes.Repeat([]byte{0x02}, 32),
				TotalAmtMsat: 8,
			},
			outcome: valid,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testUnmarshalMPP(t, test)
		})
	}
}

func testUnmarshalMPP(t *testing.T, test unmarshalMPPTest) {
	mpp, err := UnmarshalMPP(test.mpp)
	switch test.outcome {

	// Valid arguments should result in no error, a non-nil MPP record, and
	// the fields should be set correctly.
	case valid:
		if err != nil {
			t.Fatalf("unable to parse mpp record: %v", err)
		}
		if mpp == nil {
			t.Fatalf("mpp payload should be non-nil")
		}
		if int64(mpp.TotalMsat()) != test.mpp.TotalAmtMsat {
			t.Fatalf("incorrect total msat")
		}
		addr := mpp.PaymentAddr()
		if !bytes.Equal(addr[:], test.mpp.PaymentAddr) {
			t.Fatalf("incorrect payment addr")
		}

	// Invalid arguments should produce a failure and nil MPP record.
	case invalid:
		if err == nil {
			t.Fatalf("expected failure for invalid mpp")
		}
		if mpp != nil {
			t.Fatalf("mpp payload should be nil for failure")
		}

	// Arguments that produce no MPP field should return no error and no MPP
	// record.
	case nompp:
		if err != nil {
			t.Fatalf("failure for args resulting for no-mpp")
		}
		if mpp != nil {
			t.Fatalf("mpp payload should be nil for no-mpp")
		}

	default:
		t.Fatalf("test case has non-standard outcome")
	}
}
