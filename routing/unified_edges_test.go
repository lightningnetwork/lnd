package routing

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestNodeEdgeUnifier tests the composition of unified edges for nodes that
// have multiple channels between them.
func TestNodeEdgeUnifier(t *testing.T) {
	t.Parallel()

	source := route.Vertex{1}
	toNode := route.Vertex{2}
	fromNode := route.Vertex{3}
	bandwidthHints := &mockBandwidthHints{
		hints: map[uint64]lnwire.MilliSatoshi{
			100: 150,
		},
	}

	// Add two channels between the pair of nodes.
	p1 := models.CachedEdgePolicy{
		ChannelID:                 100,
		FeeProportionalMillionths: 100000,
		FeeBaseMSat:               30,
		TimeLockDelta:             60,
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		MaxHTLC:                   5000,
		MinHTLC:                   100,
	}
	p2 := models.CachedEdgePolicy{
		ChannelID:                 101,
		FeeProportionalMillionths: 190000,
		FeeBaseMSat:               10,
		TimeLockDelta:             40,
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		MaxHTLC:                   4000,
		MinHTLC:                   100,
	}
	c1 := btcutil.Amount(7)
	c2 := btcutil.Amount(8)

	inboundFee1 := models.InboundFee{
		Base: 5,
		Rate: 10000,
	}

	inboundFee2 := models.InboundFee{
		Base: 10,
		Rate: 10000,
	}

	unifierFilled := newNodeEdgeUnifier(source, toNode, false, nil)

	unifierFilled.addPolicy(
		fromNode, &p1, inboundFee1, c1, defaultHopPayloadSize, nil,
	)
	unifierFilled.addPolicy(
		fromNode, &p2, inboundFee2, c2, defaultHopPayloadSize, nil,
	)

	unifierNoCapacity := newNodeEdgeUnifier(source, toNode, false, nil)
	unifierNoCapacity.addPolicy(
		fromNode, &p1, inboundFee1, 0, defaultHopPayloadSize, nil,
	)
	unifierNoCapacity.addPolicy(
		fromNode, &p2, inboundFee2, 0, defaultHopPayloadSize, nil,
	)

	unifierNoInfo := newNodeEdgeUnifier(source, toNode, false, nil)
	unifierNoInfo.addPolicy(
		fromNode, &models.CachedEdgePolicy{}, models.InboundFee{},
		0, defaultHopPayloadSize, nil,
	)

	unifierInboundFee := newNodeEdgeUnifier(source, toNode, true, nil)
	unifierInboundFee.addPolicy(
		fromNode, &p1, inboundFee1, c1, defaultHopPayloadSize, nil,
	)
	unifierInboundFee.addPolicy(
		fromNode, &p2, inboundFee2, c2, defaultHopPayloadSize, nil,
	)

	unifierLocal := newNodeEdgeUnifier(fromNode, toNode, true, nil)
	unifierLocal.addPolicy(
		fromNode, &p1, inboundFee1, c1, defaultHopPayloadSize, nil,
	)

	inboundFeeZero := models.InboundFee{}
	inboundFeeNegative := models.InboundFee{
		Base: -150,
	}
	unifierNegInboundFee := newNodeEdgeUnifier(source, toNode, true, nil)
	unifierNegInboundFee.addPolicy(
		fromNode, &p1, inboundFeeZero, c1, defaultHopPayloadSize, nil,
	)
	unifierNegInboundFee.addPolicy(
		fromNode, &p2, inboundFeeNegative, c2, defaultHopPayloadSize,
		nil,
	)

	tests := []struct {
		name               string
		unifier            *nodeEdgeUnifier
		amount             lnwire.MilliSatoshi
		expectedFeeBase    lnwire.MilliSatoshi
		expectedFeeRate    lnwire.MilliSatoshi
		expectedInboundFee models.InboundFee
		expectedTimeLock   uint16
		expectNoPolicy     bool
		expectedCapacity   btcutil.Amount
		nextOutFee         lnwire.MilliSatoshi
	}{
		{
			name:           "amount below min htlc",
			unifier:        unifierFilled,
			amount:         50,
			expectNoPolicy: true,
		},
		{
			name:           "amount above max htlc",
			unifier:        unifierFilled,
			amount:         5500,
			expectNoPolicy: true,
		},
		// For 200 msat, p1 yields the highest fee. Use that policy to
		// forward, because it will also match p2 in case p1 does not
		// have enough balance.
		{
			name:             "use p1 with highest fee",
			unifier:          unifierFilled,
			amount:           200,
			expectedFeeBase:  p1.FeeBaseMSat,
			expectedFeeRate:  p1.FeeProportionalMillionths,
			expectedTimeLock: p1.TimeLockDelta,
			expectedCapacity: c2,
		},
		// For 400 sat, p2 yields the highest fee. Use that policy to
		// forward, because it will also match p1 in case p2 does not
		// have enough balance. In order to match p1, it needs to have
		// p1's time lock delta.
		{
			name:             "use p2 with highest fee",
			unifier:          unifierFilled,
			amount:           400,
			expectedFeeBase:  p2.FeeBaseMSat,
			expectedFeeRate:  p2.FeeProportionalMillionths,
			expectedTimeLock: p1.TimeLockDelta,
			expectedCapacity: c2,
		},
		// If there's no capacity info present, we fall back to the max
		// maxHTLC value.
		{
			name:             "no capacity info",
			unifier:          unifierNoCapacity,
			amount:           400,
			expectedFeeBase:  p2.FeeBaseMSat,
			expectedFeeRate:  p2.FeeProportionalMillionths,
			expectedTimeLock: p1.TimeLockDelta,
			expectedCapacity: p1.MaxHTLC.ToSatoshis(),
		},
		{
			name:             "no info",
			unifier:          unifierNoInfo,
			expectedCapacity: 0,
		},
		{
			name:           "local insufficient bandwidth",
			unifier:        unifierLocal,
			amount:         200,
			expectNoPolicy: true,
		},
		{
			name:               "local",
			unifier:            unifierLocal,
			amount:             100,
			expectedFeeBase:    p1.FeeBaseMSat,
			expectedFeeRate:    p1.FeeProportionalMillionths,
			expectedTimeLock:   p1.TimeLockDelta,
			expectedCapacity:   c1,
			expectedInboundFee: inboundFee1,
		},
		{
			name: "use p2 with highest fee " +
				"including inbound",
			unifier:            unifierInboundFee,
			amount:             200,
			expectedFeeBase:    p2.FeeBaseMSat,
			expectedFeeRate:    p2.FeeProportionalMillionths,
			expectedInboundFee: inboundFee2,
			expectedTimeLock:   p1.TimeLockDelta,
			expectedCapacity:   c2,
		},
		// Choose inbound fee exactly so that max htlc is just exceeded.
		// In this test, the amount that must be sent is 5001 msat.
		{
			name:           "inbound fee exceeds max htlc",
			unifier:        unifierInboundFee,
			amount:         4947,
			expectNoPolicy: true,
		},
		// The outbound fee of p2 is higher than p1, but because of the
		// inbound fee on p2 it is brought down to 0. Purely based on
		// total channel fee, p1 would be selected as the highest fee
		// channel. However, because the total node fee can never be
		// negative and the next outgoing fee is zero, the effect of the
		// inbound discount is cancelled out.
		{
			name:               "inbound fee that is rounded up",
			unifier:            unifierNegInboundFee,
			amount:             500,
			expectedFeeBase:    p2.FeeBaseMSat,
			expectedFeeRate:    p2.FeeProportionalMillionths,
			expectedInboundFee: inboundFeeNegative,
			expectedTimeLock:   p1.TimeLockDelta,
			expectedCapacity:   c2,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			edge := test.unifier.edgeUnifiers[fromNode].getEdge(
				test.amount, bandwidthHints, test.nextOutFee,
			)

			if test.expectNoPolicy {
				require.Nil(t, edge, "expected no policy")

				return
			}

			policy := edge.policy
			require.Equal(t, test.expectedFeeBase,
				policy.FeeBaseMSat, "base fee")
			require.Equal(t, test.expectedFeeRate,
				policy.FeeProportionalMillionths, "fee rate")
			require.Equal(t, test.expectedInboundFee,
				edge.inboundFees, "inbound fee")
			require.Equal(t, test.expectedTimeLock,
				policy.TimeLockDelta, "timelock")
			require.Equal(t, test.expectedCapacity, edge.capacity,
				"capacity")
		})
	}
}
