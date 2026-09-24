//go:build !test_db_postgres && test_db_sqlite

package graphdb

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestPreferredMappingDivergence verifies that the startup integrity query
// distinguishes missing, extra and incorrectly selected mappings for both
// nodes and channels without repairing them.
func TestPreferredMappingDivergence(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := sqldb.NewTestSqliteDB(t).BaseDB
	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)
	store, err := NewSQLStore(
		&SQLStoreConfig{
			ChainHash: *chaincfg.MainNetParams.GenesisHash,
			QueryCfg:  sqldb.DefaultSQLiteConfig(),
		}, executor,
	)
	require.NoError(t, err)

	graph, err := NewChannelGraph(store, WithSyncGraphCachePopulation())
	require.NoError(t, err)
	require.NoError(t, graph.Start())
	t.Cleanup(func() {
		require.NoError(t, graph.Stop())
	})

	node1Priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	node2Priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	node1V1 := createNode(t, lnwire.GossipVersion1, node1Priv)
	node1V2 := createNode(t, lnwire.GossipVersion2, node1Priv)
	node2V1 := createNode(t, lnwire.GossipVersion1, node2Priv)
	node2V2 := createNode(t, lnwire.GossipVersion2, node2Priv)
	for _, node := range []*models.Node{
		node1V1, node1V2, node2V1, node2V2,
	} {
		require.NoError(t, graph.AddNode(ctx, node))
	}

	missingChannel, _ := createEdge(
		lnwire.GossipVersion1, 800, 0, 0, 1, node1V1, node2V1,
	)
	incorrectChannelV1, _ := createEdge(
		lnwire.GossipVersion1, 801, 0, 0, 2, node1V1, node2V1,
	)
	incorrectChannelV2, _ := createEdge(
		lnwire.GossipVersion2, 801, 0, 0, 2, node1V2, node2V2,
	)
	for _, edge := range []*models.ChannelEdgeInfo{
		missingChannel, incorrectChannelV1, incorrectChannelV2,
	} {
		require.NoError(t, graph.AddChannelEdge(ctx, edge))
	}

	divergence, err := store.preferredMappingDivergence(ctx)
	require.NoError(t, err)
	require.Equal(
		t, sqlc.CountPreferredMappingDivergenceRow{}, divergence,
	)

	var node1V1ID int64
	err = db.QueryRowContext(
		ctx,
		"SELECT id FROM graph_nodes WHERE pub_key = ? AND version = ?",
		node1V1.PubKeyBytes[:], int16(lnwire.GossipVersion1),
	).Scan(&node1V1ID)
	require.NoError(t, err)

	var incorrectChannelV1ID int64
	err = db.QueryRowContext(
		ctx,
		"SELECT id FROM graph_channels WHERE scid = ? AND version = ?",
		channelIDToBytes(incorrectChannelV1.ChannelID),
		int16(lnwire.GossipVersion1),
	).Scan(&incorrectChannelV1ID)
	require.NoError(t, err)

	// Remove one mapping of each kind.
	_, err = db.ExecContext(
		ctx, "DELETE FROM graph_preferred_nodes WHERE pub_key = ?",
		node2V1.PubKeyBytes[:],
	)
	require.NoError(t, err)
	_, err = db.ExecContext(
		ctx, "DELETE FROM graph_preferred_channels WHERE scid = ?",
		channelIDToBytes(missingChannel.ChannelID),
	)
	require.NoError(t, err)

	// Point another mapping of each kind at its lower-ranked v1 row.
	_, err = db.ExecContext(
		ctx, "UPDATE graph_preferred_nodes SET node_id = ? "+
			"WHERE pub_key = ?", node1V1ID, node1V1.PubKeyBytes[:],
	)
	require.NoError(t, err)
	_, err = db.ExecContext(
		ctx, "UPDATE graph_preferred_channels SET channel_id = ? "+
			"WHERE scid = ?", incorrectChannelV1ID,
		channelIDToBytes(incorrectChannelV1.ChannelID),
	)
	require.NoError(t, err)

	// Add mappings whose keys have no corresponding graph row. Their
	// targets remain valid so the foreign keys cannot detect this case.
	extraNodeKey := make([]byte, 33)
	extraNodeKey[0] = 2
	_, err = db.ExecContext(
		ctx, "INSERT INTO graph_preferred_nodes (pub_key, node_id) "+
			"VALUES (?, ?)", extraNodeKey, node1V1ID,
	)
	require.NoError(t, err)

	extraSCID := lnwire.ShortChannelID{BlockHeight: 802}.ToUint64()
	_, err = db.ExecContext(
		ctx, "INSERT INTO graph_preferred_channels (scid, channel_id) "+
			"VALUES (?, ?)", channelIDToBytes(extraSCID),
		incorrectChannelV1ID,
	)
	require.NoError(t, err)

	want := sqlc.CountPreferredMappingDivergenceRow{
		MissingNodes:      1,
		ExtraNodes:        1,
		IncorrectNodes:    1,
		MissingChannels:   1,
		ExtraChannels:     1,
		IncorrectChannels: 1,
	}
	divergence, err = store.preferredMappingDivergence(ctx)
	require.NoError(t, err)
	require.Equal(t, want, divergence)

	// The startup check reports the divergence but deliberately leaves the
	// mappings unchanged.
	require.NoError(t, store.checkPreferredMappings(ctx))
	divergence, err = store.preferredMappingDivergence(ctx)
	require.NoError(t, err)
	require.Equal(t, want, divergence)
}
