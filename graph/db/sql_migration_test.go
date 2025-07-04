//go:build test_db_postgres || test_db_sqlite

package graphdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/color"
	"net"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

var (
	testChain         = *chaincfg.MainNetParams.GenesisHash
	testColor         = color.RGBA{R: 1, G: 2, B: 3}
	testTime          = time.Unix(11111, 0)
	testSigBytes      = testSig.Serialize()
	testExtraData     = []byte{1, 1, 1, 2, 2, 2, 2}
	testEmptyFeatures = lnwire.EmptyFeatureVector()
)

// TestMigrateGraphToSQL tests various deterministic cases that we want to test
// for to ensure that our migration from a graph store backed by a KV DB to a
// SQL database works as expected. At the end of each test, the DBs are compared
// and expected to have the exact same data in them.
func TestMigrateGraphToSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	writeUpdate := func(t *testing.T, db *KVStore, object any) {
		t.Helper()

		var err error
		switch obj := object.(type) {
		case *models.LightningNode:
			err = db.AddLightningNode(ctx, obj)
		default:
			err = fmt.Errorf("unhandled object type: %T", obj)
		}
		require.NoError(t, err)
	}

	tests := []struct {
		name          string
		write         func(t *testing.T, db *KVStore, object any)
		objects       []any
		expGraphStats graphStats
	}{
		{
			name: "empty",
		},
		{
			name:  "nodes",
			write: writeUpdate,
			//nolint:ll
			objects: []any{
				// Normal node with all fields.
				makeTestNode(t),
				// A node with no node announcement.
				makeTestShellNode(t),
				// A node with an announcement but no addresses.
				makeTestNode(t, func(n *models.LightningNode) {
					n.Addresses = nil
				}),
				// A node with all types of addresses.
				makeTestNode(t, func(n *models.LightningNode) {
					n.Addresses = []net.Addr{
						testAddr,
						testIPV4Addr,
						testIPV6Addr,
						anotherAddr,
						testOnionV2Addr,
						testOnionV3Addr,
						testOpaqueAddr,
					}
				}),
				// No extra opaque data.
				makeTestNode(t, func(n *models.LightningNode) {
					n.ExtraOpaqueData = nil
				}),
				// A node with no features.
				makeTestNode(t, func(n *models.LightningNode) {
					n.Features = lnwire.EmptyFeatureVector()
				}),
			},
			expGraphStats: graphStats{
				numNodes: 6,
			},
		},
		{
			name: "source node",
			write: func(t *testing.T, db *KVStore, object any) {
				node, ok := object.(*models.LightningNode)
				require.True(t, ok)

				err := db.SetSourceNode(ctx, node)
				require.NoError(t, err)
			},
			objects: []any{
				makeTestNode(t),
			},
			expGraphStats: graphStats{
				numNodes:   1,
				srcNodeSet: true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Set up our source kvdb DB.
			kvDB := setUpKVStore(t)

			// Write the test objects to the kvdb store.
			for _, object := range test.objects {
				test.write(t, kvDB, object)
			}

			// Set up our destination SQL DB.
			sql, ok := NewTestDB(t).(*SQLStore)
			require.True(t, ok)

			// Run the migration.
			err := MigrateGraphToSQL(
				ctx, kvDB.db, sql.db, testChain,
			)
			require.NoError(t, err)

			// Validate that the two databases are now in sync.
			assertInSync(t, kvDB, sql, test.expGraphStats)
		})
	}
}

// graphStats holds expected statistics about the graph after migration.
type graphStats struct {
	numNodes   int
	srcNodeSet bool
}

// assertInSync checks that the KVStore and SQLStore both contain the same
// graph data after migration.
func assertInSync(t *testing.T, kvDB *KVStore, sqlDB *SQLStore,
	stats graphStats) {

	// 1) Compare the nodes in the two stores.
	sqlNodes := fetchAllNodes(t, sqlDB)
	require.Len(t, sqlNodes, stats.numNodes)
	require.Equal(t, fetchAllNodes(t, kvDB), sqlNodes)

	// 2) Check that the source nodes match (if indeed source nodes have
	// been set).
	sqlSourceNode := fetchSourceNode(t, sqlDB)
	require.Equal(t, stats.srcNodeSet, sqlSourceNode != nil)
	require.Equal(t, fetchSourceNode(t, kvDB), sqlSourceNode)
}

// fetchAllNodes retrieves all nodes from the given store and returns them
// sorted by their public key.
func fetchAllNodes(t *testing.T, store V1Store) []*models.LightningNode {
	nodes := make([]*models.LightningNode, 0)

	err := store.ForEachNode(func(tx NodeRTx) error {
		node := tx.Node()

		// Call PubKey to ensure the objects cached pubkey is set so that
		// the objects can be compared as a whole.
		_, err := node.PubKey()
		require.NoError(t, err)

		nodes = append(nodes, node)

		return nil
	})
	require.NoError(t, err)

	// Sort the nodes by their public key to ensure a consistent order.
	slices.SortFunc(nodes, func(i, j *models.LightningNode) int {
		return bytes.Compare(i.PubKeyBytes[:], j.PubKeyBytes[:])
	})

	return nodes
}

// fetchSourceNode retrieves the source node from the given store.
func fetchSourceNode(t *testing.T, store V1Store) *models.LightningNode {
	node, err := store.SourceNode(context.Background())
	if errors.Is(err, ErrSourceNodeNotSet) {
		return nil
	} else {
		require.NoError(t, err)
	}

	return node
}

// setUpKVStore initializes a new KVStore for testing.
func setUpKVStore(t *testing.T) *KVStore {
	kvDB, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "graph")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	kvStore, err := NewKVStore(kvDB)
	require.NoError(t, err)

	return kvStore
}

// genPubKey generates a new public key for testing purposes.
func genPubKey(t *testing.T) route.Vertex {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var pub route.Vertex
	copy(pub[:], key.PubKey().SerializeCompressed())

	return pub
}

// testNodeOpt defines a functional option type that can be used to
// modify the attributes of a models.LightningNode crated by makeTestNode.
type testNodeOpt func(*models.LightningNode)

// makeTestNode can be used to create a test models.LightningNode. The
// functional options can be used to modify the node's attributes.
func makeTestNode(t *testing.T, opts ...testNodeOpt) *models.LightningNode {
	n := &models.LightningNode{
		HaveNodeAnnouncement: true,
		AuthSigBytes:         testSigBytes,
		LastUpdate:           testTime,
		Color:                testColor,
		Alias:                "kek",
		Features:             testFeatures,
		Addresses:            testAddrs,
		ExtraOpaqueData:      testExtraData,
		PubKeyBytes:          genPubKey(t),
	}

	for _, opt := range opts {
		opt(n)
	}

	// We call this method so that the internal pubkey field is populated
	// which then lets us to proper struct comparison later on.
	_, err := n.PubKey()
	require.NoError(t, err)

	return n
}

// makeTestShellNode creates a minimal models.LightningNode
// that only contains the public key and no other attributes.
func makeTestShellNode(t *testing.T,
	opts ...testNodeOpt) *models.LightningNode {

	n := &models.LightningNode{
		HaveNodeAnnouncement: false,
		PubKeyBytes:          genPubKey(t),
		Features:             testEmptyFeatures,
		LastUpdate:           time.Unix(0, 0),
	}

	for _, opt := range opts {
		opt(n)
	}

	// We call this method so that the internal pubkey field is populated
	// which then lets us to proper struct comparison later on.
	_, err := n.PubKey()
	require.NoError(t, err)

	return n
}

// TestMigrationWithChannelDB tests the migration of the graph store from a
// bolt backed channel.db or a kvdb channel.sqlite to a SQL database. Note that
// this test does not attempt to be a complete migration test for all graph
// store types but rather is added as a tool for developers and users to debug
// graph migration issues with an actual channel.db/channel.sqlite file.
//
// NOTE: To use this test, place either of those files in the graph/db/testdata
// directory, uncomment the "Skipf" line, and set "chain" variable appropriately
// and set the "fileName" variable to the name of the channel database file you
// want to use for the migration test.
func TestMigrationWithChannelDB(t *testing.T) {
	ctx := context.Background()

	// NOTE: comment this line out to run the test.
	t.Skipf("skipping test meant for local debugging only")

	// NOTE: set this to the genesis hash of the chain that the store
	// was created on.
	chain := *chaincfg.MainNetParams.GenesisHash

	// NOTE: set this to the name of the channel database file you want
	// to use for the migration test. This may be either a bbolt ".db" file
	// or a SQLite ".sqlite" file. If you want to migrate from a
	// bbolt channel.db file, set this to "channel.db".
	const fileName = "channel.sqlite"

	// Set up logging for the test.
	UseLogger(btclog.NewSLogger(btclog.NewDefaultHandler(os.Stdout)))

	// migrate runs the migration from the kvdb store to the SQL store.
	migrate := func(t *testing.T, kvBackend kvdb.Backend) {
		graphStore := newBatchQuerier(t)

		err := graphStore.ExecTx(
			ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
				return MigrateGraphToSQL(
					ctx, kvBackend, tx, chain,
				)
			}, sqldb.NoOpReset,
		)
		require.NoError(t, err)
	}

	connectBBolt := func(t *testing.T, dbPath string) kvdb.Backend {
		cfg := &kvdb.BoltBackendConfig{
			DBPath:            dbPath,
			DBFileName:        fileName,
			NoFreelistSync:    true,
			AutoCompact:       false,
			AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
			DBTimeout:         kvdb.DefaultDBTimeout,
		}

		kvStore, err := kvdb.GetBoltBackend(cfg)
		require.NoError(t, err)

		return kvStore
	}

	connectSQLite := func(t *testing.T, dbPath string) kvdb.Backend {
		const (
			timeout  = 10 * time.Second
			maxConns = 50
		)
		sqlbase.Init(maxConns)

		cfg := &sqlite.Config{
			Timeout:        timeout,
			BusyTimeout:    timeout,
			MaxConnections: maxConns,
		}

		kvStore, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, cfg,
			dbPath, fileName,
			// NOTE: we use the raw string here else we get an
			// import cycle if we try to import lncfg.NSChannelDB.
			"channeldb",
		)
		require.NoError(t, err)

		return kvStore
	}

	tests := []struct {
		name   string
		dbPath string
	}{
		{
			name:   "empty",
			dbPath: t.TempDir(),
		},
		{
			name:   "testdata",
			dbPath: "testdata",
		},
	}

	// Determine if we are using a SQLite file or a Bolt DB file.
	var isSqlite bool
	if strings.HasSuffix(fileName, ".sqlite") {
		isSqlite = true
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chanDBPath := path.Join(test.dbPath, fileName)
			t.Logf("Connecting to channel DB at: %s", chanDBPath)

			connectDB := connectBBolt
			if isSqlite {
				connectDB = connectSQLite
			}

			migrate(t, connectDB(t, test.dbPath))
		})
	}
}

// TestSQLMigrationEdgeCases tests various edge cases where the migration will
// still be successful but the final states of the KVStore and SQLStore
// will differ slightly.
func TestSQLMigrationEdgeCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var invalidTLVData = []byte{0x01, 0x02, 0x03}

	// Here, we test that in the case where the KV store contains a node
	// with invalid TLV data, the migration will still succeed, but the
	// node will not end up in the SQL store.
	t.Run("node with bad tlv data", func(t *testing.T) {
		// Make one valid node and one node with invalid TLV data.
		n1 := makeTestNode(t)
		n2 := makeTestNode(t, func(n *models.LightningNode) {
			n.ExtraOpaqueData = invalidTLVData
		})

		populateKV := func(t *testing.T, db *KVStore) {
			// Insert both nodes into the KV store.
			require.NoError(t, db.AddLightningNode(ctx, n1))
			require.NoError(t, db.AddLightningNode(ctx, n2))
		}

		runTestMigration(t, populateKV, dbState{
			// We expect only the valid node to be present in the
			// SQL db.
			nodes: []*models.LightningNode{n1},
		})
	})
}

// runTestMigration is a helper function that sets up the KVStore and SQLStore,
// populates the KVStore with the provided call-back, runs the migration, and
// asserts that the SQLStore contains the expected state.
func runTestMigration(t *testing.T, populateKV func(t *testing.T, db *KVStore),
	expState dbState) {

	ctx := context.Background()

	// Set up our source kvdb DB.
	kvDB := setUpKVStore(t)

	// Set up our destination SQL DB.
	sql, ok := NewTestDB(t).(*SQLStore)
	require.True(t, ok)

	// Populate the kvdb store with the test data.
	populateKV(t, kvDB)

	// Run the migration.
	err := MigrateGraphToSQL(
		ctx, kvDB.db, sql.db, testChain,
	)
	require.NoError(t, err)

	assertResultState(t, sql, expState)
}

// dbState describes the expected state of the SQLStore after a migration.
type dbState struct {
	nodes []*models.LightningNode
}

// assertResultState asserts that the SQLStore contains the expected
// state after a migration.
func assertResultState(t *testing.T, sql *SQLStore, expState dbState) {
	// Assert that the sql store contains the expected nodes.
	require.ElementsMatch(t, expState.nodes, fetchAllNodes(t, sql))
}
