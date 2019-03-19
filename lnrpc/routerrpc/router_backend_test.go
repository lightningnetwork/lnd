package routerrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	destKey       = "0286098b97bc843372b4426d4b276cea9aa2f48f0428d6f5b66ae101befc14f8b4"
	ignoreNodeKey = "02f274f48f3c0d590449a6776e3ce8825076ac376e470e992246eebc565ef8bb2a"
)

var (
	sourceKey = routing.Vertex{1, 2, 3}
)

// TestQueryRoutes asserts that query routes rpc parameters are properly parsed
// and passed onto path finding.
func TestQueryRoutes(t *testing.T) {
	ignoreNodeBytes, err := hex.DecodeString(ignoreNodeKey)
	if err != nil {
		t.Fatal(err)
	}

	var ignoreNodeVertex routing.Vertex
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
		NumRoutes:      1,
		FinalCltvDelta: 100,
		FeeLimit: &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_Fixed{
				Fixed: 250,
			},
		},
		IgnoredNodes: [][]byte{ignoreNodeBytes},
		IgnoredEdges: []*lnrpc.EdgeLocator{&lnrpc.EdgeLocator{
			ChannelId:        ignoredEdge.ChannelID,
			DirectionReverse: ignoredEdge.Direction == 1,
		}},
	}

	route := &routing.Route{}

	findRoutes := func(source, target routing.Vertex,
		amt lnwire.MilliSatoshi, restrictions *routing.RestrictParams,
		numPaths uint32, finalExpiry ...uint16) (
		[]*routing.Route, error) {

		if int64(amt) != request.Amt*1000 {
			t.Fatal("unexpected amount")
		}

		if numPaths != 1 {
			t.Fatal("unexpected number of routes")
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

		if restrictions.ProbabilitySource(routing.Vertex{}, 0,
			ignoredEdge, 0,
		) != 0 {
			t.Fatal("expecting 0% probability for ignored edge")
		}

		if restrictions.ProbabilitySource(ignoreNodeVertex, 0,
			routing.EdgeLocator{}, 0,
		) != 0 {
			t.Fatal("expecting 0% probability for ignored node")
		}

		if restrictions.ProbabilitySource(routing.Vertex{}, 0,
			routing.EdgeLocator{}, 0,
		) != 1 {
			t.Fatal("expecting 100% probability")
		}

		return []*routing.Route{
			route,
		}, nil
	}

	backend := &RouterBackend{
		MaxPaymentMSat: lnwire.NewMSatFromSatoshis(1000000),
		FindRoutes:     findRoutes,
		SelfNode:       routing.Vertex{1, 2, 3},
		FetchChannelCapacity: func(chanID uint64) (
			btcutil.Amount, error) {

			return 1, nil
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
