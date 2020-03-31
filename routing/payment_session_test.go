package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

func TestRequestRoute(t *testing.T) {
	const (
		height = 10
	)

	findPath := func(g *graphParams, r *RestrictParams,
		cfg *PathFindingConfig, source, target route.Vertex,
		amt lnwire.MilliSatoshi, finalHtlcExpiry int32) (
		[]*channeldb.ChannelEdgePolicy, error) {

		// We expect find path to receive a cltv limit excluding the
		// final cltv delta (including the block padding).
		if r.CltvLimit != 22-uint32(BlockPadding) {
			t.Fatal("wrong cltv limit")
		}

		path := []*channeldb.ChannelEdgePolicy{
			{
				Node: &channeldb.LightningNode{
					Features: lnwire.NewFeatureVector(
						nil, nil,
					),
				},
			},
		}

		return path, nil
	}

	sessionSource := &SessionSource{
		SelfNode: &channeldb.LightningNode{},
		MissionControl: &MissionControl{
			cfg: &MissionControlConfig{},
		},
	}

	cltvLimit := uint32(30)
	finalCltvDelta := uint16(8)

	payment := &LightningPayment{
		CltvLimit:      cltvLimit,
		FinalCLTVDelta: finalCltvDelta,
	}

	session := &paymentSession{
		getBandwidthHints: func() (map[uint64]lnwire.MilliSatoshi,
			error) {

			return nil, nil
		},
		sessionSource: sessionSource,
		payment:       payment,
		pathFinder:    findPath,
	}

	route, err := session.RequestRoute(height)
	if err != nil {
		t.Fatal(err)
	}

	// We expect an absolute route lock value of height + finalCltvDelta
	// + BlockPadding.
	if route.TotalTimeLock != 18+uint32(BlockPadding) {
		t.Fatalf("unexpected total time lock of %v",
			route.TotalTimeLock)
	}
}
