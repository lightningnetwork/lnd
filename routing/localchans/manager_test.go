package localchans

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
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
	const (
		newBaseFee = lnwire.MilliSatoshi(100)
		newFeeRate = uint32(200)
		newDelta   = uint32(80)
	)
	newPolicy := routing.ChannelPolicy{
		FeeSchema: routing.FeeSchema{
			BaseFee: fn.Some(newBaseFee),
			FeeRate: fn.Some(newFeeRate),
		},
		TimeLockDelta: fn.Some(newDelta),
		MaxHTLC:       5000,
	}

	currentPolicy := models.ChannelEdgePolicy{
		Version:      lnwire.GossipVersion1,
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
		if policy.TimeLockDelta != newDelta {
			t.Fatal("unexpected time lock delta")
		}
		if policy.BaseFee != newBaseFee {
			t.Fatal("unexpected base fee")
		}
		if uint32(policy.FeeRate) != newFeeRate {
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
			if uint32(policy.TimeLockDelta) != newDelta {
				t.Fatal("unexpected time lock delta")
			}
			if policy.FeeBaseMSat != newBaseFee {
				t.Fatal("unexpected base fee")
			}
			feeRate := uint32(policy.FeeProportionalMillionths)
			if feeRate != newFeeRate {
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

	fetchChannel := func(chanPoint wire.OutPoint) (*chanstate.OpenChannel,
		error) {

		if chanPoint == chanPointMissing {
			return &chanstate.OpenChannel{},
				channeldb.ErrChannelNotFound
		}

		bounds := chanstate.ChannelStateBounds{
			MaxPendingAmount: maxPendingAmount,
			MinHTLC:          minHTLC,
		}

		return &chanstate.OpenChannel{
			FundingOutpoint: chanPointValid,
			IdentityPub:     remotepub,
			LocalChanCfg: chanstate.ChannelConfig{
				ChannelStateBounds: bounds,
				MultiSigKey: keychain.KeyDescriptor{
					PubKey: localMultisigKey,
				},
			},
			RemoteChanCfg: chanstate.ChannelConfig{
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
					//nolint:ll
					edgeInfo: func() *models.ChannelEdgeInfo {
						info, err := models.NewV1Channel(
							0,
							chainhash.Hash{},
							route.Vertex{},
							route.Vertex{},
							&models.ChannelV1Fields{},
							models.WithCapacity(chanCap),
							models.WithChannelPoint(chanPointValid),
						)
						require.NoError(t, err)

						return info
					}(),
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
					//nolint:ll
					edgeInfo: func() *models.ChannelEdgeInfo {
						info, err := models.NewV1Channel(
							0,
							chainhash.Hash{},
							route.Vertex{},
							route.Vertex{},
							&models.ChannelV1Fields{},
							models.WithCapacity(chanCap),
							models.WithChannelPoint(chanPointValid),
						)
						require.NoError(t, err)

						return info
					}(),
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
					//nolint:ll
					edgeInfo: func() *models.ChannelEdgeInfo {
						info, err := models.NewV1Channel(
							0,
							chainhash.Hash{},
							route.Vertex{},
							route.Vertex{},
							&models.ChannelV1Fields{},
							models.WithCapacity(chanCap),
							models.WithChannelPoint(chanPointValid),
						)

						require.NoError(t, err)

						return info
					}(),
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
					//nolint:ll
					edgeInfo: func() *models.ChannelEdgeInfo {
						info, err := models.NewV1Channel(
							0,
							chainhash.Hash{},
							route.Vertex{},
							route.Vertex{},
							&models.ChannelV1Fields{},
							models.WithCapacity(chanCap),
							models.WithChannelPoint(chanPointValid),
						)

						require.NoError(t, err)

						return info
					}(),
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

	channel := &chanstate.OpenChannel{
		IdentityPub: remotepub,
		LocalChanCfg: chanstate.ChannelConfig{
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: localMultisigKey,
			},
		},
		RemoteChanCfg: chanstate.ChannelConfig{
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

	btcKey1 := route.NewVertex(localMultisigKey)
	btcKey2 := route.NewVertex(remoteMultisigKey)
	expectedInfo, err := models.NewV1Channel(
		8, channel.ChainHash, sp, rp, &models.ChannelV1Fields{
			BitcoinKey1Bytes: btcKey1,
			BitcoinKey2Bytes: btcKey2,
		},
		models.WithCapacity(9),
		models.WithChannelPoint(channel.FundingOutpoint),
		models.WithFundingScript(fundingScript),
	)
	require.NoError(t, err)

	expectedEdge := &models.ChannelEdgePolicy{
		Version:                   lnwire.GossipVersion1,
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

	channel := &chanstate.OpenChannel{
		IdentityPub: remotepub,
		LocalChanCfg: chanstate.ChannelConfig{
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: localMultisigKey,
			},
		},
		RemoteChanCfg: chanstate.ChannelConfig{
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

	btcKey1 := route.NewVertex(remoteMultisigKey)
	btcKey2 := route.NewVertex(localMultisigKey)
	expectedInfo, err := models.NewV1Channel(
		8, channel.ChainHash, rp, sp,
		&models.ChannelV1Fields{
			BitcoinKey1Bytes: btcKey1,
			BitcoinKey2Bytes: btcKey2,
		},
		models.WithCapacity(9),
		models.WithChannelPoint(channel.FundingOutpoint),
		models.WithFundingScript(fundingScript),
	)
	require.NoError(t, err)

	expectedEdge := &models.ChannelEdgePolicy{
		Version:                   lnwire.GossipVersion1,
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

// TestUpdatePolicyKeepFields checks that the unset fields of a policy update
// leave the matching fields of the existing edge unchanged, both on the edge
// sent to the gossiper and on the policy sent to the links.
func TestUpdatePolicyKeepFields(t *testing.T) {
	t.Parallel()

	const (
		curBaseFee  = lnwire.MilliSatoshi(500)
		curFeeRate  = lnwire.MilliSatoshi(700)
		curTimeLock = uint16(40)

		// The defaults a recreated edge starts out with.
		defBaseFee  = lnwire.MilliSatoshi(1000)
		defFeeRate  = lnwire.MilliSatoshi(2)
		defTimeLock = uint16(144)

		zeroFee = lnwire.MilliSatoshi(0)
	)

	chanPoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	info, err := models.NewV1Channel(
		0, chainhash.Hash{}, route.Vertex{}, route.Vertex{},
		&models.ChannelV1Fields{},
		models.WithCapacity(btcutil.Amount(1000)),
		models.WithChannelPoint(chanPoint),
	)
	require.NoError(t, err)

	tests := []struct {
		name         string
		policy       routing.ChannelPolicy
		missingEdge  bool
		wantBaseFee  lnwire.MilliSatoshi
		wantFeeRate  lnwire.MilliSatoshi
		wantTimeLock uint16
	}{
		{
			name:         "keep all",
			policy:       routing.ChannelPolicy{},
			wantBaseFee:  curBaseFee,
			wantFeeRate:  curFeeRate,
			wantTimeLock: curTimeLock,
		},
		{
			name: "set fee rate to zero only",
			policy: routing.ChannelPolicy{
				FeeSchema: routing.FeeSchema{
					FeeRate: fn.Some(uint32(0)),
				},
			},
			wantBaseFee:  curBaseFee,
			wantFeeRate:  0,
			wantTimeLock: curTimeLock,
		},
		{
			name: "set time lock delta only",
			policy: routing.ChannelPolicy{
				TimeLockDelta: fn.Some(uint32(80)),
			},
			wantBaseFee:  curBaseFee,
			wantFeeRate:  curFeeRate,
			wantTimeLock: 80,
		},
		{
			name: "set all fields",
			policy: routing.ChannelPolicy{
				FeeSchema: routing.FeeSchema{
					BaseFee: fn.Some(zeroFee),
					FeeRate: fn.Some(uint32(0)),
				},
				TimeLockDelta: fn.Some(uint32(80)),
			},
			wantBaseFee:  0,
			wantFeeRate:  0,
			wantTimeLock: 80,
		},
		{
			// A recreated edge has no current values, so the kept
			// fields fall back to the default policy.
			name: "missing edge keeps default fees",
			policy: routing.ChannelPolicy{
				TimeLockDelta: fn.Some(uint32(80)),
			},
			missingEdge:  true,
			wantBaseFee:  defBaseFee,
			wantFeeRate:  defFeeRate,
			wantTimeLock: 80,
		},
		{
			name:         "missing edge keeps all defaults",
			policy:       routing.ChannelPolicy{},
			missingEdge:  true,
			wantBaseFee:  defBaseFee,
			wantFeeRate:  defFeeRate,
			wantTimeLock: defTimeLock,
		},
	}

	selfPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	remotePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	bounds := chanstate.ChannelStateBounds{
		MaxPendingAmount: 999000,
		MinHTLC:          1,
	}
	openChan := &chanstate.OpenChannel{
		FundingOutpoint: chanPoint,
		IdentityPub:     remotePriv.PubKey(),
		LocalChanCfg: chanstate.ChannelConfig{
			ChannelStateBounds: bounds,
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: selfPriv.PubKey(),
			},
		},
		RemoteChanCfg: chanstate.ChannelConfig{
			ChannelStateBounds: bounds,
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: remotePriv.PubKey(),
			},
		},
	}
	fetchChannel := func(wire.OutPoint) (*chanstate.OpenChannel, error) {
		return openChan, nil
	}

	// newEdge returns the current policy of the channel.
	const maxHtlcFlag = lnwire.ChanUpdateRequiredMaxHtlc
	newEdge := func() *models.ChannelEdgePolicy {
		return &models.ChannelEdgePolicy{
			Version:                   lnwire.GossipVersion1,
			FeeBaseMSat:               curBaseFee,
			FeeProportionalMillionths: curFeeRate,
			TimeLockDelta:             curTimeLock,
			MinHTLC:                   1,
			MaxHTLC:                   5000,
			MessageFlags:              maxHtlcFlag,
		}
	}

	type fwdPolicies = map[wire.OutPoint]models.ForwardingPolicy
	type edgeCallback = func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy) error

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				gotEdges    []discovery.EdgeWithInfo
				gotPolicies fwdPolicies
			)

			manager := Manager{
				UpdateForwardingPolicies: func(p fwdPolicies) {
					gotPolicies = p
				},
				PropagateChanPolicyUpdate: func(
					e []discovery.EdgeWithInfo) error {

					gotEdges = e
					return nil
				},
				ForAllOutgoingChannels: func(_ context.Context,
					cb edgeCallback, _ func()) error {

					if tc.missingEdge {
						return nil
					}

					return cb(info, newEdge())
				},
				FetchChannel: fetchChannel,
				SelfPub:      selfPriv.PubKey(),
				DefaultRoutingPolicy: models.ForwardingPolicy{
					MinHTLCOut:    1,
					MaxHTLC:       5000,
					BaseFee:       defBaseFee,
					FeeRate:       defFeeRate,
					TimeLockDelta: uint32(defTimeLock),
				},
				AddEdge: func(context.Context,
					*models.ChannelEdgeInfo) error {

					return nil
				},
			}

			failed, err := manager.UpdatePolicy(
				t.Context(), tc.policy, tc.missingEdge,
				chanPoint,
			)
			require.NoError(t, err)
			require.Empty(t, failed)

			require.Len(t, gotEdges, 1)
			edge := gotEdges[0].Edge
			require.Equal(t, tc.wantBaseFee, edge.FeeBaseMSat)
			require.Equal(t, tc.wantFeeRate,
				edge.FeeProportionalMillionths)
			require.Equal(t, tc.wantTimeLock, edge.TimeLockDelta)

			fwd := gotPolicies[chanPoint]
			require.Equal(t, tc.wantBaseFee, fwd.BaseFee)
			require.Equal(t, tc.wantFeeRate, fwd.FeeRate)
			require.EqualValues(
				t, tc.wantTimeLock, fwd.TimeLockDelta,
			)
		})
	}
}
