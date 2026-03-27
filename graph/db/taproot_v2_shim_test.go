package graphdb

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestTimestampBlockHeightRoundTrip tests that the approximate
// timestamp↔block-height conversion round-trips within one block interval.
func TestTimestampBlockHeightRoundTrip(t *testing.T) {
	t.Parallel()

	// Pick a timestamp corresponding to a known block height.
	// Block 800000 was mined around 2023-07-24. The exact mapping
	// doesn't matter — we just verify that the round-trip is
	// within one block interval.
	original := time.Unix(1690200000, 0)

	height := timestampToApproxBlockHeight(original)
	recovered := approxBlockHeightToTimestamp(height)

	diff := original.Sub(recovered)
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, time.Duration(avgBlockInterval)*time.Second,
		"round-trip should be within one block interval")
}

// TestTimestampToBlockHeightZero tests that timestamps before genesis return
// block height 0.
func TestTimestampToBlockHeightZero(t *testing.T) {
	t.Parallel()

	require.Equal(t, uint32(0),
		timestampToApproxBlockHeight(time.Unix(0, 0)))
	require.Equal(t, uint32(0),
		timestampToApproxBlockHeight(time.Unix(
			bitcoinGenesisTimestamp-1, 0)))
}

// TestIsPrivateTaprootV2 tests the detection helper for migrated private
// taproot channels.
func TestIsPrivateTaprootV2(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		ch     sqlc.GraphChannel
		expect bool
	}{
		{
			name: "v2 no signature - private taproot",
			ch: sqlc.GraphChannel{
				Version:   int16(gossipV2),
				Signature: nil,
			},
			expect: true,
		},
		{
			name: "v2 with signature - public",
			ch: sqlc.GraphChannel{
				Version:   int16(gossipV2),
				Signature: []byte{0x01, 0x02},
			},
			expect: false,
		},
		{
			name: "v1 no signature - regular private v1",
			ch: sqlc.GraphChannel{
				Version:   int16(gossipV1),
				Signature: nil,
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expect, isPrivateTaprootV2(tt.ch))
		})
	}
}

// TestProjectV2EdgeToV1 tests that a V2 edge is correctly projected back to
// V1 with the taproot staging feature bit re-added.
func TestProjectV2EdgeToV1(t *testing.T) {
	t.Parallel()

	node1 := route.Vertex{0x01}
	node2 := route.Vertex{0x02}
	btcKey1 := route.Vertex{0x03}
	btcKey2 := route.Vertex{0x04}

	v2Edge := &models.ChannelEdgeInfo{
		Version:          lnwire.GossipVersion2,
		ChannelID:        12345,
		NodeKey1Bytes:    node1,
		NodeKey2Bytes:    node2,
		BitcoinKey1Bytes: fn.Some(btcKey1),
		BitcoinKey2Bytes: fn.Some(btcKey2),
		Features:         lnwire.EmptyFeatureVector(),
		Capacity:         1000000,
	}

	v1Edge := projectV2EdgeToV1(v2Edge)

	require.Equal(t, lnwire.GossipVersion1, v1Edge.Version)
	require.Equal(t, uint64(12345), v1Edge.ChannelID)
	require.Equal(t, node1, v1Edge.NodeKey1Bytes)
	require.Equal(t, node2, v1Edge.NodeKey2Bytes)
	require.Equal(t, fn.Some(btcKey1), v1Edge.BitcoinKey1Bytes)
	require.Equal(t, fn.Some(btcKey2), v1Edge.BitcoinKey2Bytes)
	require.Nil(t, v1Edge.AuthProof)

	// The taproot staging bit should be present.
	require.True(t, v1Edge.Features.HasFeature(
		lnwire.SimpleTaprootChannelsOptionalStaging,
	))
}

// TestProjectV2PolicyToV1 tests that a V2 policy is correctly projected back
// to V1 with approximate timestamp and V1-style flags.
func TestProjectV2PolicyToV1(t *testing.T) {
	t.Parallel()

	v2Policy := &models.ChannelEdgePolicy{
		Version:                   lnwire.GossipVersion2,
		ChannelID:                 12345,
		LastBlockHeight:           800000,
		SecondPeer:                true,
		DisableFlags:              0,
		TimeLockDelta:             40,
		MinHTLC:                   1000,
		MaxHTLC:                   500000000,
		FeeBaseMSat:               1000,
		FeeProportionalMillionths: 100,
	}

	v1Policy := projectV2PolicyToV1(v2Policy)

	require.Equal(t, lnwire.GossipVersion1, v1Policy.Version)
	require.Equal(t, uint64(12345), v1Policy.ChannelID)

	// Timestamp should be approximately correct.
	expectedTime := approxBlockHeightToTimestamp(800000)
	require.Equal(t, expectedTime, v1Policy.LastUpdate)

	// Direction bit should be set (SecondPeer = true).
	require.True(t,
		v1Policy.ChannelFlags&lnwire.ChanUpdateDirection != 0)

	// Disabled bit should NOT be set (DisableFlags = 0 = enabled).
	require.False(t, v1Policy.ChannelFlags.IsDisabled())

	// MaxHTLC flag should be set.
	require.True(t, v1Policy.MessageFlags.HasMaxHtlc())

	// Fee/HTLC fields should be preserved.
	require.Equal(t, uint16(40), v1Policy.TimeLockDelta)
	require.Equal(t, lnwire.MilliSatoshi(1000), v1Policy.MinHTLC)
	require.Equal(t, lnwire.MilliSatoshi(500000000), v1Policy.MaxHTLC)
	require.Equal(t, lnwire.MilliSatoshi(1000), v1Policy.FeeBaseMSat)
	require.Equal(t, lnwire.MilliSatoshi(100),
		v1Policy.FeeProportionalMillionths)
}

// TestProjectV2PolicyToV1Disabled tests that a disabled V2 policy correctly
// sets the disabled flag in the V1 projection.
func TestProjectV2PolicyToV1Disabled(t *testing.T) {
	t.Parallel()

	v2Policy := &models.ChannelEdgePolicy{
		Version:         lnwire.GossipVersion2,
		LastBlockHeight: 800000,
		DisableFlags: lnwire.ChanUpdateDisableIncoming |
			lnwire.ChanUpdateDisableOutgoing,
	}

	v1Policy := projectV2PolicyToV1(v2Policy)
	require.True(t, v1Policy.ChannelFlags.IsDisabled())
}

// TestProjectV1PolicyToV2 tests that a V1 policy is correctly converted to
// V2 representation for writing to a migrated V2 channel.
func TestProjectV1PolicyToV2(t *testing.T) {
	t.Parallel()

	ts := time.Unix(1690200000, 0)
	v1Policy := &models.ChannelEdgePolicy{
		Version:                   lnwire.GossipVersion1,
		ChannelID:                 12345,
		LastUpdate:                ts,
		ChannelFlags:              lnwire.ChanUpdateDirection | lnwire.ChanUpdateDisabled, //nolint:ll
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		TimeLockDelta:             40,
		MinHTLC:                   1000,
		MaxHTLC:                   500000000,
		FeeBaseMSat:               1000,
		FeeProportionalMillionths: 100,
	}

	v2Policy := projectV1PolicyToV2(v1Policy)

	require.Equal(t, lnwire.GossipVersion2, v2Policy.Version)
	require.Equal(t, uint64(12345), v2Policy.ChannelID)

	// Block height should be approximately correct.
	expectedHeight := timestampToApproxBlockHeight(ts)
	require.Equal(t, expectedHeight, v2Policy.LastBlockHeight)

	// Direction: ChanUpdateDirection set means SecondPeer = true.
	require.True(t, v2Policy.SecondPeer)

	// Disabled: both incoming and outgoing should be disabled.
	require.True(t, v2Policy.DisableFlags.IncomingDisabled())
	require.True(t, v2Policy.DisableFlags.OutgoingDisabled())

	// Fee/HTLC fields should be preserved.
	require.Equal(t, uint16(40), v2Policy.TimeLockDelta)
	require.Equal(t, lnwire.MilliSatoshi(1000), v2Policy.MinHTLC)
	require.Equal(t, lnwire.MilliSatoshi(500000000), v2Policy.MaxHTLC)
	require.Equal(t, lnwire.MilliSatoshi(1000), v2Policy.FeeBaseMSat)
	require.Equal(t, lnwire.MilliSatoshi(100),
		v2Policy.FeeProportionalMillionths)
}

// TestProjectV2PolicyToV1Nil tests that nil policies are handled gracefully.
func TestProjectV2PolicyToV1Nil(t *testing.T) {
	t.Parallel()

	require.Nil(t, projectV2PolicyToV1(nil))
	require.Nil(t, projectV1PolicyToV2(nil))
}

// TestFeatureVectorWithoutTaproot tests that the taproot staging bits are
// stripped while other bits are preserved.
func TestFeatureVectorWithoutTaproot(t *testing.T) {
	t.Parallel()

	fv := lnwire.EmptyFeatureVector()
	fv.Set(lnwire.SimpleTaprootChannelsRequiredStaging)
	fv.Set(lnwire.SimpleTaprootChannelsOptionalStaging)
	fv.Set(lnwire.StaticRemoteKeyRequired) // Should survive.

	result := featureVectorWithoutTaproot(fv)

	require.False(t, result.HasFeature(
		lnwire.SimpleTaprootChannelsOptionalStaging,
	))
	require.True(t, result.HasFeature(
		lnwire.StaticRemoteKeyRequired,
	))
}

// TestV1EdgeIsPrivateTaproot tests the V1 workaround detection.
func TestV1EdgeIsPrivateTaproot(t *testing.T) {
	t.Parallel()

	// V1 edge with taproot feature bit.
	fv := lnwire.EmptyFeatureVector()
	fv.Set(lnwire.SimpleTaprootChannelsRequiredStaging)

	taprootEdge := &models.ChannelEdgeInfo{
		Version:  lnwire.GossipVersion1,
		Features: fv,
	}
	require.True(t, v1EdgeIsPrivateTaproot(taprootEdge))

	// V1 edge without taproot feature bit.
	normalEdge := &models.ChannelEdgeInfo{
		Version:  lnwire.GossipVersion1,
		Features: lnwire.EmptyFeatureVector(),
	}
	require.False(t, v1EdgeIsPrivateTaproot(normalEdge))

	// V2 edge should not match.
	v2Edge := &models.ChannelEdgeInfo{
		Version:  lnwire.GossipVersion2,
		Features: fv,
	}
	require.False(t, v1EdgeIsPrivateTaproot(v2Edge))
}
