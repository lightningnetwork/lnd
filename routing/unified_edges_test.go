package routing

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
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
	bandwidthHints := &mockBandwidthHints{}

	// Add two channels between the pair of nodes.
	p1 := channeldb.CachedEdgePolicy{
		FeeProportionalMillionths: 100000,
		FeeBaseMSat:               30,
		TimeLockDelta:             60,
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		MaxHTLC:                   5000,
		MinHTLC:                   100,
	}
	p2 := channeldb.CachedEdgePolicy{
		FeeProportionalMillionths: 190000,
		FeeBaseMSat:               10,
		TimeLockDelta:             40,
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		MaxHTLC:                   4000,
		MinHTLC:                   100,
	}
	c1 := btcutil.Amount(7)
	c2 := btcutil.Amount(8)

	unifierFilled := newNodeEdgeUnifier(source, toNode, nil)
	unifierFilled.addPolicy(fromNode, &p1, c1)
	unifierFilled.addPolicy(fromNode, &p2, c2)

	unifierNoCapacity := newNodeEdgeUnifier(source, toNode, nil)
	unifierNoCapacity.addPolicy(fromNode, &p1, 0)
	unifierNoCapacity.addPolicy(fromNode, &p2, 0)

	unifierNoInfo := newNodeEdgeUnifier(source, toNode, nil)
	unifierNoInfo.addPolicy(fromNode, &channeldb.CachedEdgePolicy{}, 0)

	tests := []struct {
		name             string
		unifier          *nodeEdgeUnifier
		amount           lnwire.MilliSatoshi
		expectedFeeBase  lnwire.MilliSatoshi
		expectedFeeRate  lnwire.MilliSatoshi
		expectedTimeLock uint16
		expectNoPolicy   bool
		expectedCapacity btcutil.Amount
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
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			edge := test.unifier.edgeUnifiers[fromNode].getEdge(
				test.amount, bandwidthHints,
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
			require.Equal(t, test.expectedTimeLock,
				policy.TimeLockDelta, "timelock")
			require.Equal(t, test.expectedCapacity, edge.capacity,
				"capacity")
		})
	}
}
