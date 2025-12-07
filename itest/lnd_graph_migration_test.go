package itest

import (
	"context"
	"database/sql"
	"net"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// testGraphMigration tests that the graph migration from the old KV store to
// the new native SQL store works as expected.
func testGraphMigration(ht *lntest.HarnessTest) {
	ctx := ht.Context()
	alice := ht.NewNodeWithCoins("Alice", nil)

	// Make sure we run the test with SQLite or Postgres.
	if alice.Cfg.DBBackend != node.BackendSqlite &&
		alice.Cfg.DBBackend != node.BackendPostgres {

		ht.Skip("node not running with SQLite or Postgres")
	}

	// Skip the test if the node is already running with native SQL.
	if alice.Cfg.NativeSQL {
		ht.Skip("node already running with native SQL")
	}

	// Spin up a mini network and then connect Alice to it.
	chans, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil, nil, nil},
		lntest.OpenChannelParams{Amt: chanAmt},
	)

	// The expected number of nodes in the graph will include those spun up
	// above plus Alice.
	expNumNodes := len(nodes) + 1
	expNumChans := len(chans)
	require.Equal(ht, 5, expNumNodes)
	require.Equal(ht, 3, expNumChans)

	// Connect Alice to one of the nodes. Alice should now perform a graph
	// sync with the node.
	ht.EnsureConnected(alice, nodes[0])

	// Wait for Alice to have a full view of the graph.
	ht.AssertNumEdges(alice, expNumChans, false)

	// Now stop Alice so we can open the DB for examination.
	require.NoError(ht, alice.Stop())

	// Open the KV store channel graph DB.
	db, err := graphdb.NewKVStore(openKVBackend(ht, alice))
	require.NoError(ht, err)

	// assertDBState is a helper function that asserts the state of the
	// graph DB.
	assertDBState := func(db graphdb.V1Store) {
		var (
			numNodes int
			edges    = make(map[uint64]bool)
		)
		err := db.ForEachNodeCached(ctx, false, func(_ context.Context,
			_ route.Vertex, _ []net.Addr,
			chans map[uint64]*graphdb.DirectedChannel) error {

			numNodes++

			// For each node, also count the number of edges.
			for _, ch := range chans {
				edges[ch.ChannelID] = true
			}

			return nil
		}, func() {
			clear(edges)
			numNodes = 0
		})
		require.NoError(ht, err)
		require.Equal(ht, expNumNodes, numNodes)
		require.Equal(ht, expNumChans, len(edges))
	}
	assertDBState(db)

	alice.SetExtraArgs([]string{"--db.use-native-sql"})

	// Now run the migration flow three times to ensure that each run is
	// idempotent.
	for i := 0; i < 3; i++ {
		// Start Alice with the native SQL flag set. This will trigger
		// the migration to run.
		require.NoError(ht, alice.Start(ht.Context()))

		// At this point the migration should have completed and the
		// node should be running with native SQL. Now we'll stop Alice
		// again so we can safely examine the database.
		require.NoError(ht, alice.Stop())

		// Now we'll open the database with the native SQL backend and
		// fetch the graph data again to ensure that it was migrated
		// correctly.
		sqlGraphDB := openNativeSQLGraphDB(ht, alice)
		assertDBState(sqlGraphDB)
	}

	// Now restart Alice without the --db.use-native-sql flag so we can
	// check that the KV tombstone was set and that Alice will fail to
	// start.
	// NOTE: this is the same tombstone used for the graph migration. Only
	// one tombstone is needed since we just need one to represent the fact
	// that the switch to native SQL has been made.
	require.NoError(ht, alice.Stop())
	alice.SetExtraArgs(nil)

	// Alice should now fail to start due to the tombstone being set.
	require.NoError(ht, alice.StartLndCmd(ht.Context()))
	require.ErrorContains(ht, alice.WaitForProcessExit(), "exit status 1")

	// Start Alice again so the test can complete.
	alice.SetExtraArgs([]string{"--db.use-native-sql"})
	require.NoError(ht, alice.Start(ht.Context()))
}

func openNativeSQLGraphDB(ht *lntest.HarnessTest,
	hn *node.HarnessNode) graphdb.V1Store {

	db := openNativeSQLDB(ht, hn)

	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) graphdb.SQLQueries {
			return db.WithTx(tx)
		},
	)

	queryCfg := sqldb.DefaultSQLiteConfig()
	if hn.Cfg.DBBackend != node.BackendSqlite {
		queryCfg = sqldb.DefaultPostgresConfig()
	}

	store, err := graphdb.NewSQLStore(
		&graphdb.SQLStoreConfig{
			ChainHash: *ht.Miner().ActiveNet.GenesisHash,
			QueryCfg:  queryCfg,
		},
		executor,
	)
	require.NoError(ht, err)

	return store
}
