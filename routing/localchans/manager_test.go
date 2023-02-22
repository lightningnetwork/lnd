package localchans

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

// TestManager tests that the local channel manager properly propagates fee
// updates to gossiper and links.
func TestManager(t *testing.T) {
	t.Parallel()

	type channel struct {
		edgeInfo *channeldb.ChannelEdgeInfo
	}

	var (
		chanPointValid     = wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
		chanCap            = btcutil.Amount(1000)
		chanPointMissing   = wire.OutPoint{Hash: chainhash.Hash{2}, Index: 2}
		maxPendingAmount   = lnwire.MilliSatoshi(999000)
		minHTLC            = lnwire.MilliSatoshi(2000)
		expectedNumUpdates int
		channelSet         []channel
	)

	newPolicy := routing.ChannelPolicy{
		FeeSchema: routing.FeeSchema{
			BaseFee: 100,
			FeeRate: 200,
		},
		TimeLockDelta: 80,
		MaxHTLC:       5000,
	}

	currentPolicy := channeldb.ChannelEdgePolicy{
		MinHTLC:      minHTLC,
		MessageFlags: lnwire.ChanUpdateRequiredMaxHtlc,
	}

	updateForwardingPolicies := func(
		chanPolicies map[wire.OutPoint]htlcswitch.ForwardingPolicy) {

		if len(chanPolicies) == 0 {
			return
		}

		if len(chanPolicies) != 1 {
			t.Fatal("unexpected number of policies to apply")
		}

		policy := chanPolicies[chanPointValid]
		if policy.TimeLockDelta != newPolicy.TimeLockDelta {
			t.Fatal("unexpected time lock delta")
		}
		if policy.BaseFee != newPolicy.BaseFee {
			t.Fatal("unexpected base fee")
		}
		if uint32(policy.FeeRate) != newPolicy.FeeRate {
			t.Fatal("unexpected base fee")
		}
		if policy.MaxHTLC != newPolicy.MaxHTLC {
			t.Fatal("unexpected max htlc")
		}
	}

	propagateChanPolicyUpdate := func(
		edgesToUpdate []discovery.EdgeWithInfo) error {

		if len(edgesToUpdate) != expectedNumUpdates {
			t.Fatalf("unexpected number of updates,"+
				" expected %d got %d", expectedNumUpdates,
				len(edgesToUpdate))
		}

		for _, edge := range edgesToUpdate {
			policy := edge.Edge
			if !policy.MessageFlags.HasMaxHtlc() {
				t.Fatal("expected max htlc flag")
			}
			if policy.TimeLockDelta != uint16(newPolicy.TimeLockDelta) {
				t.Fatal("unexpected time lock delta")
			}
			if policy.FeeBaseMSat != newPolicy.BaseFee {
				t.Fatal("unexpected base fee")
			}
			if uint32(policy.FeeProportionalMillionths) != newPolicy.FeeRate {
				t.Fatal("unexpected base fee")
			}
			if policy.MaxHTLC != newPolicy.MaxHTLC {
				t.Fatal("unexpected max htlc")
			}
		}

		return nil
	}

	forAllOutgoingChannels := func(cb func(kvdb.RTx,
		*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy) error) error {

		for _, c := range channelSet {
			if err := cb(nil, c.edgeInfo, &currentPolicy); err != nil {
				return err
			}
		}
		return nil
	}

	fetchChannel := func(tx kvdb.RTx, chanPoint wire.OutPoint) (
		*channeldb.OpenChannel, error) {

		if chanPoint == chanPointMissing {
			return &channeldb.OpenChannel{}, channeldb.ErrChannelNotFound
		}

		constraints := channeldb.ChannelConstraints{
			MaxPendingAmount: maxPendingAmount,
			MinHTLC:          minHTLC,
		}

		return &channeldb.OpenChannel{
			LocalChanCfg: channeldb.ChannelConfig{
				ChannelConstraints: constraints,
			},
		}, nil
	}

	manager := Manager{
		UpdateForwardingPolicies:  updateForwardingPolicies,
		PropagateChanPolicyUpdate: propagateChanPolicyUpdate,
		ForAllOutgoingChannels:    forAllOutgoingChannels,
		FetchChannel:              fetchChannel,
	}

	// Policy with no max htlc value.
	MaxHTLCPolicy := currentPolicy
	MaxHTLCPolicy.MaxHTLC = newPolicy.MaxHTLC
	noMaxHtlcPolicy := newPolicy
	noMaxHtlcPolicy.MaxHTLC = 0

	tests := []struct {
		name                   string
		currentPolicy          channeldb.ChannelEdgePolicy
		newPolicy              routing.ChannelPolicy
		channelSet             []channel
		specifiedChanPoints    []wire.OutPoint
		expectedNumUpdates     int
		expectedUpdateFailures []lnrpc.UpdateFailure
		expectErr              error
	}{
		{
			name:          "valid channel",
			currentPolicy: currentPolicy,
			newPolicy:     newPolicy,
			channelSet: []channel{
				{
					edgeInfo: &channeldb.ChannelEdgeInfo{
						Capacity:     chanCap,
						ChannelPoint: chanPointValid,
					},
				},
			},
			specifiedChanPoints:    []wire.OutPoint{chanPointValid},
			expectedNumUpdates:     1,
			expectedUpdateFailures: []lnrpc.UpdateFailure{},
			expectErr:              nil,
		},
		{
			name:          "all channels",
			currentPolicy: currentPolicy,
			newPolicy:     newPolicy,
			channelSet: []channel{
				{
					edgeInfo: &channeldb.ChannelEdgeInfo{
						Capacity:     chanCap,
						ChannelPoint: chanPointValid,
					},
				},
			},
			specifiedChanPoints:    []wire.OutPoint{},
			expectedNumUpdates:     1,
			expectedUpdateFailures: []lnrpc.UpdateFailure{},
			expectErr:              nil,
		},
		{
			name:          "missing channel",
			currentPolicy: currentPolicy,
			newPolicy:     newPolicy,
			channelSet: []channel{
				{
					edgeInfo: &channeldb.ChannelEdgeInfo{
						Capacity:     chanCap,
						ChannelPoint: chanPointValid,
					},
				},
			},
			specifiedChanPoints: []wire.OutPoint{chanPointMissing},
			expectedNumUpdates:  0,
			expectedUpdateFailures: []lnrpc.UpdateFailure{
				lnrpc.UpdateFailure_UPDATE_FAILURE_NOT_FOUND,
			},
			expectErr: nil,
		},
		{
			// Here, no max htlc is specified, the max htlc value
			// should be kept unchanged.
			name:          "no max htlc specified",
			currentPolicy: MaxHTLCPolicy,
			newPolicy:     noMaxHtlcPolicy,
			channelSet: []channel{
				{
					edgeInfo: &channeldb.ChannelEdgeInfo{
						Capacity:     chanCap,
						ChannelPoint: chanPointValid,
					},
				},
			},
			specifiedChanPoints:    []wire.OutPoint{chanPointValid},
			expectedNumUpdates:     1,
			expectedUpdateFailures: []lnrpc.UpdateFailure{},
			expectErr:              nil,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			currentPolicy = test.currentPolicy
			channelSet = test.channelSet
			expectedNumUpdates = test.expectedNumUpdates

			failedUpdates, err := manager.UpdatePolicy(test.newPolicy,
				test.specifiedChanPoints...)

			if len(failedUpdates) != len(test.expectedUpdateFailures) {
				t.Fatalf("wrong number of failed policy updates")
			}

			if len(test.expectedUpdateFailures) > 0 {
				if failedUpdates[0].Reason != test.expectedUpdateFailures[0] {
					t.Fatalf("wrong expected policy update failure")
				}
			}

			require.Equal(t, test.expectErr, err)
		})
	}
}
