package localchans

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/routing"
)

// TestManager tests that the local channel manager properly propagates fee
// updates to gossiper and links.
func TestManager(t *testing.T) {
	chanPoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	chanCap := btcutil.Amount(1000)

	newPolicy := routing.ChannelPolicy{
		FeeSchema: routing.FeeSchema{
			BaseFee: 100,
			FeeRate: 200,
		},
		TimeLockDelta: 80,
	}

	updateForwardingPolicies := func(
		chanPolicies map[wire.OutPoint]htlcswitch.ForwardingPolicy) {

		if len(chanPolicies) != 1 {
			t.Fatal("unexpected number of policies to apply")
		}

		policy := chanPolicies[chanPoint]
		if policy.TimeLockDelta != newPolicy.TimeLockDelta {
			t.Fatal("unexpected time lock delta")
		}
		if policy.BaseFee != newPolicy.BaseFee {
			t.Fatal("unexpected base fee")
		}
		if uint32(policy.FeeRate) != newPolicy.FeeRate {
			t.Fatal("unexpected base fee")
		}
		if policy.MaxHTLC != lnwire.NewMSatFromSatoshis(chanCap) {
			t.Fatal("unexpected max htlc")
		}
	}

	propagateChanPolicyUpdate := func(
		edgesToUpdate []discovery.EdgeWithInfo) error {

		if len(edgesToUpdate) != 1 {
			t.Fatal("unexpected number of edges to update")
		}

		policy := edgesToUpdate[0].Edge
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
		if policy.MaxHTLC != lnwire.NewMSatFromSatoshis(chanCap) {
			t.Fatal("unexpected max htlc")
		}

		return nil
	}

	forAllOutgoingChannels := func(cb func(*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy) error) error {

		return cb(
			&channeldb.ChannelEdgeInfo{
				Capacity:     chanCap,
				ChannelPoint: chanPoint,
			},
			&channeldb.ChannelEdgePolicy{},
		)
	}

	manager := Manager{
		UpdateForwardingPolicies:  updateForwardingPolicies,
		PropagateChanPolicyUpdate: propagateChanPolicyUpdate,
		ForAllOutgoingChannels:    forAllOutgoingChannels,
	}

	// Test updating a specific channels.
	err := manager.UpdatePolicy(newPolicy, chanPoint)
	if err != nil {
		t.Fatal(err)
	}

	// Test updating all channels, which comes down to the same as testing a
	// specific channel because there is only one channel.
	err = manager.UpdatePolicy(newPolicy)
	if err != nil {
		t.Fatal(err)
	}
}
