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
)

var (
	sourceKey = route.Vertex{1, 2, 3}
)

// TestQueryRoutes asserts that query routes rpc parameters are properly parsed
// and passed onto path finding.
func TestQueryRoutes(t *testing.T) {
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
		IgnoredEdges: []*lnrpc.EdgeLocator{{
			ChannelId:        555,
			DirectionReverse: true,
		}},
	}

	rt := &route.Route{}

	findRoutes := func(source, target route.Vertex,
		amt lnwire.MilliSatoshi, restrictions *routing.RestrictParams,
		numPaths uint32, finalExpiry ...uint16) (
		[]*route.Route, error) {

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

		if len(restrictions.IgnoredEdges) != 1 {
			t.Fatal("unexpected ignored edges map size")
		}

		if _, ok := restrictions.IgnoredEdges[routing.EdgeLocator{
			ChannelID: 555, Direction: 1,
		}]; !ok {
			t.Fatal("unexpected ignored edge")
		}

		if len(restrictions.IgnoredNodes) != 1 {
			t.Fatal("unexpected ignored nodes map size")
		}

		if _, ok := restrictions.IgnoredNodes[ignoreNodeVertex]; !ok {
			t.Fatal("unexpected ignored node")
		}

		return []*route.Route{
			rt,
		}, nil
	}

	backend := &RouterBackend{
		MaxPaymentMSat: lnwire.NewMSatFromSatoshis(1000000),
		FindRoutes:     findRoutes,
		SelfNode:       route.Vertex{1, 2, 3},
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
