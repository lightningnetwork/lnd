//go:build test_db_postgres || test_db_sqlite

package migration1

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/graph/db/migration1/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestEdgeInfoFeatureRoundTrip asserts that the migration reader accepts the
// largest raw feature vector that the legacy writer can produce.
func TestEdgeInfoFeatureRoundTrip(t *testing.T) {
	store := setUpKVStore(t)

	newVertex := func() route.Vertex {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return route.NewVertex(key.PubKey())
	}
	node1, node2 := newVertex(), newVertex()
	rawFeatures := lnwire.NewRawFeatureVector(
		lnwire.FeatureBit(math.MaxUint16),
	)
	edge := &models.ChannelEdgeInfo{
		ChannelID:        1,
		NodeKey1Bytes:    node1,
		NodeKey2Bytes:    node2,
		BitcoinKey1Bytes: node1,
		BitcoinKey2Bytes: node2,
		Features: lnwire.NewFeatureVector(
			rawFeatures, lnwire.Features,
		),
	}
	require.NoError(t, store.AddChannelEdge(t.Context(), edge))

	var stored *models.ChannelEdgeInfo
	err := store.ForEachChannel(
		t.Context(),
		func(info *models.ChannelEdgeInfo,
			_ *models.ChannelEdgePolicy,
			_ *models.ChannelEdgePolicy) error {

			stored = info
			return nil
		}, func() {
			stored = nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.True(t, stored.Features.Equals(rawFeatures))
}

// TestEdgePolicyOpaqueDataRoundTrip asserts that the migration reader accepts
// the largest opaque policy blob that the legacy writer can produce.
func TestEdgePolicyOpaqueDataRoundTrip(t *testing.T) {
	var (
		extra   bytes.Buffer
		scratch [8]byte
	)
	require.NoError(t, tlv.WriteVarInt(&extra, 1, &scratch))
	require.NoError(t, tlv.WriteVarInt(&extra, 9996, &scratch))
	_, err := extra.Write(make([]byte, 9996))
	require.NoError(t, err)
	require.Equal(t, MaxAllowedExtraOpaqueBytes, extra.Len())

	policy := &models.ChannelEdgePolicy{
		MessageFlags:    lnwire.ChanUpdateRequiredMaxHtlc,
		ExtraOpaqueData: extra.Bytes(),
	}

	var encoded bytes.Buffer
	require.NoError(t, serializeChanEdgePolicy(
		&encoded, policy, policy.ToNode[:],
	))

	decoded, err := deserializeChanEdgePolicy(&encoded)
	require.NoError(t, err)
	require.Equal(t, policy.MessageFlags, decoded.MessageFlags)
	require.Equal(t, policy.MaxHTLC, decoded.MaxHTLC)
	require.Equal(t, policy.ExtraOpaqueData, decoded.ExtraOpaqueData)
}
