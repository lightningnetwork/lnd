package graphdb

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestMigratePrivateTaprootToV2 tests the full migration of a private taproot
// channel from V1 workaround storage to canonical V2.
func TestMigratePrivateTaprootToV2(t *testing.T) {
	t.Parallel()

	if !isSQLDB {
		t.Skip("migration test requires SQL backend")
	}

	ctx := t.Context()
	store := NewTestDB(t)

	// Create two test nodes and a source node.
	sourceNode := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, store.AddNode(ctx, sourceNode))
	require.NoError(t, store.SetSourceNode(ctx, sourceNode))

	node1 := createTestVertex(t, lnwire.GossipVersion1)
	node2 := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, store.AddNode(ctx, node1))
	require.NoError(t, store.AddNode(ctx, node2))

	// Create a V1 channel with the taproot staging feature bit and
	// NO auth proof (simulating a private taproot channel).
	edgeInfo, shortChanID := createEdge(
		lnwire.GossipVersion1, 100, 1, 0, 0,
		node1, node2,
		true, // skipProof = true (private channel).
	)

	// Set the taproot staging feature bit on the channel.
	taprootFeatures := lnwire.NewRawFeatureVector(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	)
	edgeInfo.Features = lnwire.NewFeatureVector(
		taprootFeatures, lnwire.Features,
	)

	require.NoError(t, store.AddChannelEdge(ctx, edgeInfo))

	// Add a policy for the channel.
	edgePolicy := &models.ChannelEdgePolicy{
		Version:                   lnwire.GossipVersion1,
		ChannelID:                 shortChanID.ToUint64(),
		LastUpdate:                time.Unix(1690200000, 0),
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		ChannelFlags:              0, // Node1 direction, enabled.
		TimeLockDelta:             40,
		MinHTLC:                   1000,
		MaxHTLC:                   500000000,
		FeeBaseMSat:               1000,
		FeeProportionalMillionths: 100,
	}
	_, _, err := store.UpdateEdgePolicy(ctx, edgePolicy)
	require.NoError(t, err)

	// Verify the V1 channel exists before migration.
	chanID := shortChanID.ToUint64()
	dbEdge, p1, _, err := store.FetchChannelEdgesByID(
		ctx, lnwire.GossipVersion1, chanID,
	)
	require.NoError(t, err)
	require.Equal(t, lnwire.GossipVersion1, dbEdge.Version)
	require.True(t, dbEdge.Features.HasFeature(
		lnwire.SimpleTaprootChannelsOptionalStaging,
	))
	require.Nil(t, dbEdge.AuthProof)
	require.NotNil(t, p1)

	// Run the migration.
	sqlStore, ok := store.(*SQLStore)
	require.True(t, ok, "test requires SQL store")
	require.NoError(t, sqlStore.RunTaprootV2Migration(ctx))

	// After migration, the V1 workaround row is gone and a V2 row
	// exists. Thanks to the shim, querying as V1 should still work.
	dbEdge2, p1After, _, err := store.FetchChannelEdgesByID(
		ctx, lnwire.GossipVersion1, chanID,
	)
	require.NoError(t, err)

	// The shim projects V2 back to V1, so the version should be V1
	// with the taproot feature bit re-added.
	require.Equal(t, lnwire.GossipVersion1, dbEdge2.Version)
	require.True(t, dbEdge2.Features.HasFeature(
		lnwire.SimpleTaprootChannelsOptionalStaging,
	))
	require.Nil(t, dbEdge2.AuthProof)

	// Verify the policy survived (projected back to V1).
	require.NotNil(t, p1After)
	require.Equal(t, uint16(40), p1After.TimeLockDelta)
	require.Equal(t, lnwire.MilliSatoshi(1000), p1After.FeeBaseMSat)
	require.Equal(t, lnwire.MilliSatoshi(100),
		p1After.FeeProportionalMillionths)

	// HasChannelEdge should still find it.
	exists, isZombie, err := store.HasChannelEdge(
		ctx, lnwire.GossipVersion1, chanID,
	)
	require.NoError(t, err)
	require.True(t, exists)
	require.False(t, isZombie)

	// ChannelView should include it (for startup chain filter).
	edgePoints, err := store.ChannelView(
		ctx, lnwire.GossipVersion1,
	)
	require.NoError(t, err)

	found := false
	for _, ep := range edgePoints {
		if ep.OutPoint == edgeInfo.ChannelPoint {
			found = true

			break
		}
	}
	require.True(t, found, "migrated channel should appear in "+
		"ChannelView")
}

// TestMigrateIdempotent verifies that running the migration twice is safe.
func TestMigrateIdempotent(t *testing.T) {
	t.Parallel()

	if !isSQLDB {
		t.Skip("migration test requires SQL backend")
	}

	ctx := t.Context()
	store := NewTestDB(t)

	sourceNode := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, store.AddNode(ctx, sourceNode))
	require.NoError(t, store.SetSourceNode(ctx, sourceNode))

	node1 := createTestVertex(t, lnwire.GossipVersion1)
	node2 := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, store.AddNode(ctx, node1))
	require.NoError(t, store.AddNode(ctx, node2))

	// Create a private taproot V1 channel.
	edgeInfo, _ := createEdge(
		lnwire.GossipVersion1, 200, 1, 0, 0,
		node1, node2, true,
	)
	taprootFeatures := lnwire.NewRawFeatureVector(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	)
	edgeInfo.Features = lnwire.NewFeatureVector(
		taprootFeatures, lnwire.Features,
	)
	require.NoError(t, store.AddChannelEdge(ctx, edgeInfo))

	sqlStore := store.(*SQLStore)

	// Run migration twice — second run should be a no-op.
	require.NoError(t, sqlStore.RunTaprootV2Migration(ctx))
	require.NoError(t, sqlStore.RunTaprootV2Migration(ctx))
}

// TestMigrateNonTaprootUntouched verifies that regular V1 channels are not
// affected by the migration.
func TestMigrateNonTaprootUntouched(t *testing.T) {
	t.Parallel()

	if !isSQLDB {
		t.Skip("migration test requires SQL backend")
	}

	ctx := t.Context()
	store := NewTestDB(t)

	sourceNode := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, store.AddNode(ctx, sourceNode))
	require.NoError(t, store.SetSourceNode(ctx, sourceNode))

	node1 := createTestVertex(t, lnwire.GossipVersion1)
	node2 := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, store.AddNode(ctx, node1))
	require.NoError(t, store.AddNode(ctx, node2))

	// Create a regular V1 channel (no taproot feature bit, with proof).
	edgeInfo, shortChanID := createEdge(
		lnwire.GossipVersion1, 300, 1, 0, 0,
		node1, node2,
	)
	require.NoError(t, store.AddChannelEdge(ctx, edgeInfo))

	sqlStore := store.(*SQLStore)
	require.NoError(t, sqlStore.RunTaprootV2Migration(ctx))

	// The regular channel should still be V1 and unchanged.
	dbEdge, _, _, err := store.FetchChannelEdgesByID(
		ctx, lnwire.GossipVersion1, shortChanID.ToUint64(),
	)
	require.NoError(t, err)
	require.Equal(t, lnwire.GossipVersion1, dbEdge.Version)
	require.False(t, dbEdge.Features.HasFeature(
		lnwire.SimpleTaprootChannelsOptionalStaging,
	))
	require.NotNil(t, dbEdge.AuthProof)
}
