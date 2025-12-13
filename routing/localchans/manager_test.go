package localchans

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/keychain"
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
		edgeInfo *models.ChannelEdgeInfo
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

	sp := [33]byte{}
	_, _ = hex.Decode(sp[:], []byte("028d7500dd4c12685d1f568b4c2b5048e85"+
		"34b873319f3a8daa612b469132ec7f7"))
	rp := [33]byte{}
	_, _ = hex.Decode(rp[:], []byte("034f355bdcb7cc0af728ef3cceb9615d906"+
		"84bb5b2ca5f859ab0f0b704075871aa"))
	selfpub, _ := btcec.ParsePubKey(sp[:])
	remotepub, _ := btcec.ParsePubKey(rp[:])
	localMultisigPrivKey, _ := btcec.NewPrivateKey()
	localMultisigKey := localMultisigPrivKey.PubKey()
	remoteMultisigPrivKey, _ := btcec.NewPrivateKey()
	remoteMultisigKey := remoteMultisigPrivKey.PubKey()
	newPolicy := routing.ChannelPolicy{
		FeeSchema: routing.FeeSchema{
			BaseFee: 100,
			FeeRate: 200,
		},
		TimeLockDelta: 80,
		MaxHTLC:       5000,
	}

	currentPolicy := models.ChannelEdgePolicy{
		MinHTLC:      minHTLC,
		MessageFlags: lnwire.ChanUpdateRequiredMaxHtlc,
	}

	updateForwardingPolicies := func(
		chanPolicies map[wire.OutPoint]models.ForwardingPolicy) {

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

	forAllOutgoingChannels := func(_ context.Context,
		cb func(*models.ChannelEdgeInfo,
			*models.ChannelEdgePolicy) error, _ func()) error {

		for _, c := range channelSet {
			if err := cb(c.edgeInfo, &currentPolicy); err != nil {
				return err
			}
		}
		return nil
	}

	fetchChannel := func(chanPoint wire.OutPoint) (*channeldb.OpenChannel,
		error) {

		if chanPoint == chanPointMissing {
			return &channeldb.OpenChannel{}, channeldb.ErrChannelNotFound
		}

		bounds := channeldb.ChannelStateBounds{
			MaxPendingAmount: maxPendingAmount,
			MinHTLC:          minHTLC,
		}

		return &channeldb.OpenChannel{
			FundingOutpoint: chanPointValid,
			IdentityPub:     remotepub,
			LocalChanCfg: channeldb.ChannelConfig{
				ChannelStateBounds: bounds,
				MultiSigKey: keychain.KeyDescriptor{
					PubKey: localMultisigKey,
				},
			},
			RemoteChanCfg: channeldb.ChannelConfig{
				ChannelStateBounds: bounds,
				MultiSigKey: keychain.KeyDescriptor{
					PubKey: remoteMultisigKey,
				},
			},
		}, nil
	}

	addEdge := func(_ context.Context, _ *models.ChannelEdgeInfo) error {
		return nil
	}

	manager := Manager{
		UpdateForwardingPolicies:  updateForwardingPolicies,
		PropagateChanPolicyUpdate: propagateChanPolicyUpdate,
		ForAllOutgoingChannels:    forAllOutgoingChannels,
		FetchChannel:              fetchChannel,
		SelfPub:                   selfpub,
		DefaultRoutingPolicy: models.ForwardingPolicy{
			MinHTLCOut:    minHTLC,
			MaxHTLC:       maxPendingAmount,
			BaseFee:       lnwire.MilliSatoshi(1000),
			FeeRate:       lnwire.MilliSatoshi(0),
			InboundFee:    models.InboundFee{},
			TimeLockDelta: 80,
		},
		AddEdge: addEdge,
	}

	// Policy with no max htlc value.
	MaxHTLCPolicy := currentPolicy
	MaxHTLCPolicy.MaxHTLC = newPolicy.MaxHTLC
	noMaxHtlcPolicy := newPolicy
	noMaxHtlcPolicy.MaxHTLC = 0

	tests := []struct {
		name                   string
		currentPolicy          models.ChannelEdgePolicy
		newPolicy              routing.ChannelPolicy
		channelSet             []channel
		specifiedChanPoints    []wire.OutPoint
		createMissingEdge      bool
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
					edgeInfo: &models.ChannelEdgeInfo{
						Capacity:     chanCap,
						ChannelPoint: chanPointValid,
					},
				},
			},
			specifiedChanPoints:    []wire.OutPoint{chanPointValid},
			createMissingEdge:      false,
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
					edgeInfo: &models.ChannelEdgeInfo{
						Capacity:     chanCap,
						ChannelPoint: chanPointValid,
					},
				},
			},
			specifiedChanPoints:    []wire.OutPoint{},
			createMissingEdge:      false,
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
					edgeInfo: &models.ChannelEdgeInfo{
						Capacity:     chanCap,
						ChannelPoint: chanPointValid,
					},
				},
			},
			specifiedChanPoints: []wire.OutPoint{chanPointMissing},
			createMissingEdge:   false,
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
					edgeInfo: &models.ChannelEdgeInfo{
						Capacity:     chanCap,
						ChannelPoint: chanPointValid,
					},
				},
			},
			specifiedChanPoints:    []wire.OutPoint{chanPointValid},
			createMissingEdge:      false,
			expectedNumUpdates:     1,
			expectedUpdateFailures: []lnrpc.UpdateFailure{},
			expectErr:              nil,
		},
		{
			// Here, the edge is missing, causing the edge to be
			// recreated.
			name:                   "missing edge recreated",
			currentPolicy:          models.ChannelEdgePolicy{},
			newPolicy:              newPolicy,
			channelSet:             []channel{},
			specifiedChanPoints:    []wire.OutPoint{chanPointValid},
			createMissingEdge:      true,
			expectedNumUpdates:     1,
			expectedUpdateFailures: []lnrpc.UpdateFailure{},
			expectErr:              nil,
		},
		{
			// Here, the edge is missing, but the edge will not be
			// recreated, because createMissingEdge is false.
			name:                "missing edge ignored",
			currentPolicy:       models.ChannelEdgePolicy{},
			newPolicy:           newPolicy,
			channelSet:          []channel{},
			specifiedChanPoints: []wire.OutPoint{chanPointValid},
			createMissingEdge:   false,
			expectedNumUpdates:  0,
			expectedUpdateFailures: []lnrpc.UpdateFailure{
				lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN,
			},
			expectErr: nil,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			currentPolicy = test.currentPolicy
			channelSet = test.channelSet
			expectedNumUpdates = test.expectedNumUpdates

			failedUpdates, err := manager.UpdatePolicy(
				t.Context(),
				test.newPolicy,
				test.createMissingEdge,
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

// Tests creating a new edge in the manager where the local pubkey is the
// lexicographically lesser or the two.
func TestCreateEdgeLower(t *testing.T) {
	sp := [33]byte{}
	_, _ = hex.Decode(sp[:], []byte("028d7500dd4c12685d1f568b4c2b5048e85"+
		"34b873319f3a8daa612b469132ec7f7"))
	rp := [33]byte{}
	_, _ = hex.Decode(rp[:], []byte("034f355bdcb7cc0af728ef3cceb9615d906"+
		"84bb5b2ca5f859ab0f0b704075871aa"))
	selfpub, _ := btcec.ParsePubKey(sp[:])
	remotepub, _ := btcec.ParsePubKey(rp[:])
	localMultisigPrivKey, _ := btcec.NewPrivateKey()
	localMultisigKey := localMultisigPrivKey.PubKey()
	remoteMultisigPrivKey, _ := btcec.NewPrivateKey()
	remoteMultisigKey := remoteMultisigPrivKey.PubKey()
	timestamp := time.Now()
	defaultPolicy := models.ForwardingPolicy{
		MinHTLCOut: 1,
		MaxHTLC:    2,
		BaseFee:    3,
		FeeRate:    4,
		InboundFee: models.InboundFee{
			Base: 5,
			Rate: 6,
		},
		TimeLockDelta: 7,
	}

	channel := &channeldb.OpenChannel{
		IdentityPub: remotepub,
		LocalChanCfg: channeldb.ChannelConfig{
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: localMultisigKey,
			},
		},
		RemoteChanCfg: channeldb.ChannelConfig{
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: remoteMultisigKey,
			},
		},
		ShortChannelID: lnwire.NewShortChanIDFromInt(8),
		ChainHash:      *chaincfg.RegressionNetParams.GenesisHash,
		Capacity:       9,
		FundingOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash([32]byte{}),
			Index: 0,
		},
	}

	fundingScript, err := funding.MakeFundingScript(channel)
	require.NoError(t, err)

	expectedInfo := &models.ChannelEdgeInfo{
		ChannelID:     8,
		ChainHash:     channel.ChainHash,
		Features:      lnwire.EmptyFeatureVector(),
		Capacity:      9,
		ChannelPoint:  channel.FundingOutpoint,
		NodeKey1Bytes: sp,
		NodeKey2Bytes: rp,
		BitcoinKey1Bytes: [33]byte(
			localMultisigKey.SerializeCompressed()),
		BitcoinKey2Bytes: [33]byte(
			remoteMultisigKey.SerializeCompressed()),
		AuthProof:       nil,
		ExtraOpaqueData: nil,
		FundingScript:   fn.Some(fundingScript),
	}
	expectedEdge := &models.ChannelEdgePolicy{
		ChannelID:                 8,
		LastUpdate:                timestamp,
		TimeLockDelta:             7,
		ChannelFlags:              0,
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		FeeBaseMSat:               3,
		FeeProportionalMillionths: 4,
		MinHTLC:                   1,
		MaxHTLC:                   2,
		SigBytes:                  nil,
		ToNode:                    rp,
		ExtraOpaqueData:           nil,
	}
	manager := Manager{
		SelfPub:              selfpub,
		DefaultRoutingPolicy: defaultPolicy,
	}

	info, edge, err := manager.createEdge(channel, timestamp)
	require.NoError(t, err)
	require.Equal(t, expectedInfo, info)
	require.Equal(t, expectedEdge, edge)
}

// Tests creating a new edge in the manager where the local pubkey is the
// lexicographically higher or the two.
func TestCreateEdgeHigher(t *testing.T) {
	sp := [33]byte{}
	_, _ = hex.Decode(sp[:], []byte("034f355bdcb7cc0af728ef3cceb9615d906"+
		"84bb5b2ca5f859ab0f0b704075871aa"))
	rp := [33]byte{}
	_, _ = hex.Decode(rp[:], []byte("028d7500dd4c12685d1f568b4c2b5048e85"+
		"34b873319f3a8daa612b469132ec7f7"))
	selfpub, _ := btcec.ParsePubKey(sp[:])
	remotepub, _ := btcec.ParsePubKey(rp[:])
	localMultisigPrivKey, _ := btcec.NewPrivateKey()
	localMultisigKey := localMultisigPrivKey.PubKey()
	remoteMultisigPrivKey, _ := btcec.NewPrivateKey()
	remoteMultisigKey := remoteMultisigPrivKey.PubKey()
	timestamp := time.Now()
	defaultPolicy := models.ForwardingPolicy{
		MinHTLCOut: 1,
		MaxHTLC:    2,
		BaseFee:    3,
		FeeRate:    4,
		InboundFee: models.InboundFee{
			Base: 5,
			Rate: 6,
		},
		TimeLockDelta: 7,
	}

	channel := &channeldb.OpenChannel{
		IdentityPub: remotepub,
		LocalChanCfg: channeldb.ChannelConfig{
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: localMultisigKey,
			},
		},
		RemoteChanCfg: channeldb.ChannelConfig{
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: remoteMultisigKey,
			},
		},
		ShortChannelID: lnwire.NewShortChanIDFromInt(8),
		ChainHash:      *chaincfg.RegressionNetParams.GenesisHash,
		Capacity:       9,
		FundingOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash([32]byte{}),
			Index: 0,
		},
	}

	fundingScript, err := funding.MakeFundingScript(channel)
	require.NoError(t, err)

	expectedInfo := &models.ChannelEdgeInfo{
		ChannelID:     8,
		ChainHash:     channel.ChainHash,
		Features:      lnwire.EmptyFeatureVector(),
		Capacity:      9,
		ChannelPoint:  channel.FundingOutpoint,
		NodeKey1Bytes: rp,
		NodeKey2Bytes: sp,
		BitcoinKey1Bytes: [33]byte(
			remoteMultisigKey.SerializeCompressed()),
		BitcoinKey2Bytes: [33]byte(
			localMultisigKey.SerializeCompressed()),
		AuthProof:       nil,
		ExtraOpaqueData: nil,
		FundingScript:   fn.Some(fundingScript),
	}
	expectedEdge := &models.ChannelEdgePolicy{
		ChannelID:                 8,
		LastUpdate:                timestamp,
		TimeLockDelta:             7,
		ChannelFlags:              1,
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		FeeBaseMSat:               3,
		FeeProportionalMillionths: 4,
		MinHTLC:                   1,
		MaxHTLC:                   2,
		SigBytes:                  nil,
		ToNode:                    rp,
		ExtraOpaqueData:           nil,
	}
	manager := Manager{
		SelfPub:              selfpub,
		DefaultRoutingPolicy: defaultPolicy,
	}

	info, edge, err := manager.createEdge(channel, timestamp)
	require.NoError(t, err)
	require.Equal(t, expectedInfo, info)
	require.Equal(t, expectedEdge, edge)
}
