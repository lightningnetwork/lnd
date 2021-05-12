package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

func TestValidateCLTVLimit(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		cltvLimit      uint32
		finalCltvDelta uint16
		includePadding bool
		expectError    bool
	}{
		{
			name:           "bad limit with padding",
			cltvLimit:      uint32(103),
			finalCltvDelta: uint16(100),
			includePadding: true,
			expectError:    true,
		},
		{
			name:           "good limit with padding",
			cltvLimit:      uint32(104),
			finalCltvDelta: uint16(100),
			includePadding: true,
			expectError:    false,
		},
		{
			name:           "bad limit no padding",
			cltvLimit:      uint32(100),
			finalCltvDelta: uint16(100),
			includePadding: false,
			expectError:    true,
		},
		{
			name:           "good limit no padding",
			cltvLimit:      uint32(101),
			finalCltvDelta: uint16(100),
			includePadding: false,
			expectError:    false,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		success := t.Run(testCase.name, func(t *testing.T) {
			err := ValidateCLTVLimit(
				testCase.cltvLimit, testCase.finalCltvDelta,
				testCase.includePadding,
			)

			if testCase.expectError {
				require.NotEmpty(t, err)
			} else {
				require.NoError(t, err)
			}
		})
		if !success {
			break
		}
	}
}

func TestRequestRoute(t *testing.T) {
	const (
		height = 10
	)

	cltvLimit := uint32(30)
	finalCltvDelta := uint16(8)

	payment := &LightningPayment{
		CltvLimit:      cltvLimit,
		FinalCLTVDelta: finalCltvDelta,
		Amount:         1000,
		FeeLimit:       1000,
	}

	var paymentHash [32]byte
	if err := payment.SetPaymentHash(paymentHash); err != nil {
		t.Fatal(err)
	}

	session, err := newPaymentSession(
		payment,
		func() (map[uint64]lnwire.MilliSatoshi,
			error) {

			return nil, nil
		},
		func() (routingGraph, func(), error) {
			return &sessionGraph{}, func() {}, nil
		},
		&MissionControl{},
		PathFindingConfig{},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Override pathfinder with a mock.
	session.pathFinder = func(
		g *graphParams, r *RestrictParams, cfg *PathFindingConfig,
		source, target route.Vertex, amt lnwire.MilliSatoshi,
		finalHtlcExpiry int32) ([]*channeldb.ChannelEdgePolicy, error) {

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

	route, err := session.RequestRoute(
		payment.Amount, payment.FeeLimit, 0, height,
	)
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

type sessionGraph struct {
	routingGraph
}

func (g *sessionGraph) sourceNode() route.Vertex {
	return route.Vertex{}
}
