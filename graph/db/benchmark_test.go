package graphdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

// Here we define various database paths, connection strings and file names that
// we will use to open the database connections. These should be changed to
// point to your actual local test databases.
const (
	bboltDBPath          = "testdata/kvdb"
	kvdbSqlitePath       = "testdata/kvdb"
	nativeSQLSqlitePath  = "testdata"
	kvdbPostgresDNS      = "postgres://test@localhost/graphbenchmark_kvdb"
	nativeSQLPostgresDNS = "postgres://test@localhost/graphbenchmark"

	kvdbSqliteFile      = "channel.sqlite"
	kvdbBBoltFile       = "channel.db"
	nativeSQLSqliteFile = "lnd.sqlite"

	testMaxSQLiteConnections   = 2
	testMaxPostgresConnections = 50
	testSQLBusyTimeout         = 5 * time.Second
)

// Here we define some variables that will be used to configure the graph stores
// we open for testing. These can be modified to suit your testing needs.
var (
	// dbTestChain is the chain hash used for initialising the test
	// databases. This should be changed to match the chain hash of the
	// database you are testing against.
	dbTestChain = *chaincfg.MainNetParams.GenesisHash

	// testStoreOptions is used to configure the graph stores we open for
	// testing.
	testStoreOptions = []StoreOptionModifier{
		WithBatchCommitInterval(500 * time.Millisecond),
	}

	// testSqlitePragmaOpts is a set of SQLite pragma options that we apply
	// to the SQLite databases we open for testing.
	testSqlitePragmaOpts = []string{
		"synchronous=full",
		"auto_vacuum=incremental",
		"fullfsync=true",
	}
)

// dbConnection is a struct that holds the name of the database connection
// and a function to open the connection.
type dbConnection struct {
	name string
	open func(testing.TB) V1Store
}

// This var block defines the various database connections that we will use
// for testing. Each connection is defined as a dbConnection struct that
// contains a name and an open function. The open function is used to create
// a new V1Store instance for the given database type.
var (
	// kvdbBBoltConn is a connection to a kvdb-bbolt database called
	// channel.db.
	kvdbBBoltConn = dbConnection{
		name: "kvdb-bbolt",
		open: func(b testing.TB) V1Store {
			return connectBBoltDB(b, bboltDBPath, kvdbBBoltFile)
		},
	}

	// kvdbSqliteConn is a connection to a kvdb-sqlite database called
	// channel.sqlite.
	kvdbSqliteConn = dbConnection{
		name: "kvdb-sqlite",
		open: func(b testing.TB) V1Store {
			return connectKVDBSqlite(
				b, kvdbSqlitePath, kvdbSqliteFile,
			)
		},
	}

	// nativeSQLSqliteConn is a connection to a native SQL sqlite database
	// called lnd.sqlite.
	nativeSQLSqliteConn = dbConnection{
		name: "native-sqlite",
		open: func(b testing.TB) V1Store {
			return connectNativeSQLite(
				b, sqldb.DefaultSQLiteConfig(),
				nativeSQLSqlitePath, nativeSQLSqliteFile,
			)
		},
	}

	// kvdbPostgresConn is a connection to a kvdb-postgres database
	// using a postgres connection string.
	kvdbPostgresConn = dbConnection{
		name: "kvdb-postgres",
		open: func(b testing.TB) V1Store {
			return connectKVDBPostgres(b, kvdbPostgresDNS)
		},
	}

	// nativeSQLPostgresConn is a connection to a native SQL postgres
	// database using a postgres connection string.
	nativeSQLPostgresConn = dbConnection{
		name: "native-postgres",
		open: func(b testing.TB) V1Store {
			return connectNativePostgres(
				b, sqldb.DefaultPostgresConfig(),
				nativeSQLPostgresDNS,
			)
		},
	}
)

// connectNativePostgres creates a V1Store instance backed by a native Postgres
// database for testing purposes.
func connectNativePostgres(t testing.TB, cfg *sqldb.QueryConfig,
	dsn string) V1Store {

	return newSQLStore(t, cfg, sqlPostgres(t, dsn))
}

// sqlPostgres creates a sqldb.DB instance backed by a native Postgres database
// for testing purposes.
func sqlPostgres(t testing.TB, dsn string) BatchedSQLQueries {
	store, err := sqldb.NewPostgresStore(&sqldb.PostgresConfig{
		Dsn:            dsn,
		MaxConnections: testMaxPostgresConnections,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	return newSQLExecutor(t, store)
}

// connectNativeSQLite creates a V1Store instance backed by a native SQLite
// database for testing purposes.
func connectNativeSQLite(t testing.TB, cfg *sqldb.QueryConfig, dbPath,
	file string) V1Store {

	return newSQLStore(t, cfg, sqlSQLite(t, dbPath, file))
}

// sqlSQLite creates a sqldb.DB instance backed by a native SQLite database for
// testing purposes.
func sqlSQLite(t testing.TB, dbPath, file string) BatchedSQLQueries {
	store, err := sqldb.NewSqliteStore(
		&sqldb.SqliteConfig{
			MaxConnections: testMaxSQLiteConnections,
			BusyTimeout:    testSQLBusyTimeout,
			PragmaOptions:  testSqlitePragmaOpts,
		},
		path.Join(dbPath, file),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	return newSQLExecutor(t, store)
}

// kvdbPostgres creates a kvdb.Backend instance backed by a kvdb-postgres
// database for testing purposes.
func kvdbPostgres(t testing.TB, dsn string) kvdb.Backend {
	kvStore, err := kvdb.Open(
		kvdb.PostgresBackendName, context.Background(),
		&postgres.Config{
			Dsn:            dsn,
			MaxConnections: testMaxPostgresConnections,
		},
		// NOTE: we use the raw string here else we get an
		// import cycle if we try to import lncfg.NSChannelDB.
		"channeldb",
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, kvStore.Close())
	})

	return kvStore
}

// connectKVDBPostgres creates a V1Store instance backed by a kvdb-postgres
// database for testing purposes.
func connectKVDBPostgres(t testing.TB, dsn string) V1Store {
	return newKVStore(t, kvdbPostgres(t, dsn))
}

// kvdbSqlite creates a kvdb.Backend instance backed by a kvdb-sqlite
// database for testing purposes.
func kvdbSqlite(t testing.TB, dbPath, fileName string) kvdb.Backend {
	sqlbase.Init(testMaxSQLiteConnections)
	kvStore, err := kvdb.Open(
		kvdb.SqliteBackendName, context.Background(),
		&sqlite.Config{
			BusyTimeout:    testSQLBusyTimeout,
			MaxConnections: testMaxSQLiteConnections,
			PragmaOptions:  testSqlitePragmaOpts,
		}, dbPath, fileName,
		// NOTE: we use the raw string here else we get an
		// import cycle if we try to import lncfg.NSChannelDB.
		"channeldb",
	)
	require.NoError(t, err)

	return kvStore
}

// connectKVDBSqlite creates a V1Store instance backed by a kvdb-sqlite
// database for testing purposes.
func connectKVDBSqlite(t testing.TB, dbPath, fileName string) V1Store {
	return newKVStore(t, kvdbSqlite(t, dbPath, fileName))
}

// connectBBoltDB creates a new BBolt database connection for testing.
func connectBBoltDB(t testing.TB, dbPath, fileName string) V1Store {
	return newKVStore(t, kvdbBBolt(t, dbPath, fileName))
}

// kvdbBBolt creates a new bbolt backend for testing.
func kvdbBBolt(t testing.TB, dbPath, fileName string) kvdb.Backend {
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

// newKVStore creates a new KVStore instance for testing using a provided
// kvdb.Backend instance.
func newKVStore(t testing.TB, backend kvdb.Backend) V1Store {
	store, err := NewKVStore(backend, testStoreOptions...)
	require.NoError(t, err)

	return store
}

// newSQLExecutor creates a new BatchedSQLQueries instance for testing using a
// provided sqldb.DB instance.
func newSQLExecutor(t testing.TB, db sqldb.DB) BatchedSQLQueries {
	err := db.ApplyAllMigrations(
		context.Background(), sqldb.GetMigrations(),
	)
	require.NoError(t, err)

	return sqldb.NewTransactionExecutor(
		db.GetBaseDB(), func(tx *sql.Tx) SQLQueries {
			return db.GetBaseDB().WithTx(tx)
		},
	)
}

// newSQLStore creates a new SQLStore instance for testing using a provided
// sqldb.DB instance.
func newSQLStore(t testing.TB, cfg *sqldb.QueryConfig,
	db BatchedSQLQueries) V1Store {

	store, err := NewSQLStore(
		&SQLStoreConfig{
			ChainHash: dbTestChain,
			QueryCfg:  cfg,
		},
		db, testStoreOptions...,
	)
	require.NoError(t, err)

	return store
}

// TestPopulateDBs is a helper test that can be used to populate various local
// graph DBs from some source graph DB. This can then be used to run the
// various benchmark tests against the same graph data.
//
// TODO(elle): this test reveals that the batching logic we use might be
// problematic for postgres backends. This needs some investigation & it might
// make sense to only use LazyAdd for sqlite backends & it may make sense to
// also add a maximum batch size to avoid grouping too many updates at once.
// Observations:
//   - the LazyAdd options need to be turned off for channel/policy update calls
//     for both the native SQL postgres & kvdb postgres backend for this test to
//     succeed.
//   - The LazyAdd option must be added for the sqlite backends, else it takes
//     very long to sync the graph.
func TestPopulateDBs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// NOTE: uncomment the line below to run this test locally, then provide
	// the desired source database (and make sure the destination Postgres
	// databases exist and are running).
	t.Skipf("Skipping local helper test")

	// Set your desired source database here. For kvdbSqliteConn, a file
	// called testdata/kvdb/channel.sqlite must exist and be populated with
	// a KVDB based channel graph.
	sourceDB := kvdbSqliteConn

	// Populate this list with the desired destination databases.
	destinations := []dbConnection{
		kvdbBBoltConn,
		nativeSQLSqliteConn,
		kvdbPostgresConn,
		nativeSQLPostgresConn,
	}

	// Open and start the source graph.
	src, err := NewChannelGraph(sourceDB.open(t))
	require.NoError(t, err)
	require.NoError(t, src.Start())
	t.Cleanup(func() {
		require.NoError(t, src.Stop())
	})

	// countNodes is a helper function to count the number of nodes in the
	// graph.
	countNodes := func(graph *ChannelGraph) int {
		numNodes := 0
		err := graph.ForEachNode(
			ctx, func(node *models.LightningNode) error {
				numNodes++

				return nil
			}, func() {
				numNodes = 0
			},
		)
		require.NoError(t, err)

		return numNodes
	}

	// countChannels is a helper function to count the number of channels
	// in the graph.
	countChannels := func(graph *ChannelGraph) (int, int) {
		var (
			numChans    = 0
			numPolicies = 0
		)
		err := graph.ForEachChannel(
			ctx, func(info *models.ChannelEdgeInfo,
				policy,
				policy2 *models.ChannelEdgePolicy) error {

				numChans++
				if policy != nil {
					numPolicies++
				}
				if policy2 != nil {
					numPolicies++
				}

				return nil
			}, func() {
				numChans = 0
				numPolicies = 0
			})
		require.NoError(t, err)

		return numChans, numPolicies
	}

	t.Logf("Number of nodes in source graph (%s): %d", sourceDB.name,
		countNodes(src))
	numChan, numPol := countChannels(src)
	t.Logf("Number of channels & policies in source graph (%s): %d "+
		"channels, %d policies", sourceDB.name, numChan, numPol)

	for _, destDB := range destinations {
		t.Run(destDB.name, func(t *testing.T) {
			t.Parallel()

			// Open and start the destination graph.
			dest, err := NewChannelGraph(destDB.open(t))
			require.NoError(t, err)
			require.NoError(t, dest.Start())
			t.Cleanup(func() {
				require.NoError(t, dest.Stop())
			})

			t.Logf("Number of nodes in %s graph: %d", destDB.name,
				countNodes(dest))
			numChan, numPol := countChannels(dest)
			t.Logf("Number of channels in %s graph: %d, %d",
				destDB.name, numChan, numPol)

			// Sync the source graph to the destination graph.
			syncGraph(t, src, dest)

			t.Logf("Number of nodes in %s graph after sync: %d",
				destDB.name, countNodes(dest))
			numChan, numPol = countChannels(dest)
			t.Logf("Number of channels in %s graph after sync: "+
				"%d, %d", destDB.name, numChan, numPol)
		})
	}
}

// TestPopulateViaMigration is a helper test that can be used to populate a
// local native SQL graph from a kvdbgraph using the migration logic.
//
// NOTE: the testPostgres variable can be set to true to test with a
// postgres backend instead of the kvdb-sqlite backend.
//
// NOTE: this is a helper test and is not run by default.
func TestPopulateViaMigration(t *testing.T) {
	// ======= STEP 0 ===========
	// Comment out this SKipf line.
	t.Skipf("Skipping local helper test")

	const (
		srcBBolt    = "kvdb-bbolt"
		srcSQLite   = "kvdb-sqlite"
		srcPostgres = "kvdb-postgres"
	)

	// ======= STEP 1 ===========
	// Set your chosen SOURCE type by uncommenting the corresponding line
	// below. By default, a kvdb-sqlite source is chosen.
	srcDB := srcSQLite
	// srcDB := srcBBolt
	// srcDB := srcPostgres

	// ======= STEP 2 ============
	// Set this variable to the correct genesis hash of the source
	// DB. By default, mainnet is assumed.
	chain := *chaincfg.MainNetParams.GenesisHash

	// ======= STEP 3 (ignore if source is postgres) ==============
	// If your source destination is bbolt or sqlite, then set this to the
	// path where your source database can be found.
	const sourceDBPath = "testdata"

	// ======= STEP 4 (only if source is bbolt!) ============
	// If your source destination is bbolt, then set this to the name of
	// the bbolt file that contains the channel graph data.
	const sourceBBoltName = "channel.db"

	// ======= STEP 5 (only if source is sqlite!) ============
	// If your source destination is sqlite, then set this to the name of
	// the sqlite file that contains the channel graph data.
	const sourceSQLiteName = "channel.sqlite"

	// ======= STEP 6 (only if source is postgres!) ============
	// Set the DNS of your kvdb postgres instance below. This should be the
	// same as what you have set in the config of the LND node that
	// populated the instance (ie, whatever your --db.postgres.dsn is set
	// to).
	const kvdbPostgresDNS = "postgres://user@host/db_name"

	// ======== STEP 7 ========================
	// Finally, pick your destination DB! You can choose either SQLite or
	// Postgres.
	testSQLite := true

	// ======== STEP 8 (only if destination is sqlite) ========
	// Set the path where you want to create the destination SQLite
	// database. This should be a directory that exists and is writable.
	const destSQLitePath = "testdata"

	// ======== STEP 9 (only if destination is sqlite) ========
	// Pick a name for your destination SQLite database file.
	// NOTE: if you run this test again, delete the previously created
	// file first.
	const destSQLiteFile = "lnd-graph-test.sqlite"

	// ======== STEP 10 (only if destination is postgres) ========
	// NB: this has some additional steps:
	// 1. First, connect to your destination postgres instance: example:
	//	 $ psql -U ellemouton -d postgres
	// 2. Now, create the test database:
	//	CREATE DATABASE graphtest;
	// NOTE: if you restart this test for postgres, it helps to first drop
	// the new database & recreate it.
	// NOTE: the database name that you use above must be whatever you will
	// use in the DNS you set below.
	const sqlPostgresDNS = "postgres://user@host/graphtest"

	// ======= YOUR WORK IS DONE =============

	// Connect to source database.
	var srcKVDB kvdb.Backend
	switch srcDB {
	case srcBBolt:
		srcKVDB = kvdbBBolt(t, sourceDBPath, sourceBBoltName)
	case srcSQLite:
		srcKVDB = kvdbSqlite(t, sourceDBPath, sourceSQLiteName)
	case srcPostgres:
		srcKVDB = kvdbPostgres(t, kvdbPostgresDNS)
	default:
		t.Fatalf("Unsupported source database backend: %s", srcDB)
	}

	// Connect to destination database.
	cfg := sqldb.DefaultSQLiteConfig()
	dstSQL := sqlSQLite(t, destSQLitePath, destSQLiteFile)
	if !testSQLite {
		cfg = sqldb.DefaultPostgresConfig()
		dstSQL = sqlPostgres(t, sqlPostgresDNS)
	}

	// Set up a logger so we can see the migration progress.
	logger := btclog.NewDefaultHandler(os.Stdout)
	UseLogger(btclog.NewSLogger(logger))
	log.SetLevel(btclog.LevelDebug)

	// Use the graph migration to populate the SQL graph from the
	// kvdb graph.
	ctx := context.Background()
	err := dstSQL.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(queries SQLQueries) error {
			return MigrateGraphToSQL(
				ctx, &SQLStoreConfig{
					QueryCfg:  cfg,
					ChainHash: chain,
				}, srcKVDB, queries,
			)
		}, func() {},
	)
	require.NoError(t, err)
}

// syncGraph synchronizes the source graph with the destination graph by
// copying all nodes and channels from the source to the destination.
func syncGraph(t *testing.T, src, dest *ChannelGraph) {
	ctx := context.Background()

	var (
		s = rate.Sometimes{
			Interval: 10 * time.Second,
		}
		t0 = time.Now()

		chunk = 0
		total = 0
		mu    sync.Mutex
	)

	reportNodeStats := func() {
		elapsed := time.Since(t0).Seconds()
		ratePerSec := float64(chunk) / elapsed
		t.Logf("Synced %d nodes (last chunk: %d) "+
			"(%.2f nodes/second)",
			total, chunk, ratePerSec)

		t0 = time.Now()
	}

	var wgNodes sync.WaitGroup
	err := src.ForEachNode(ctx, func(node *models.LightningNode) error {
		wgNodes.Add(1)
		go func() {
			defer wgNodes.Done()

			err := dest.AddLightningNode(ctx, node, batch.LazyAdd())
			require.NoError(t, err)

			mu.Lock()
			total++
			chunk++
			s.Do(func() {
				reportNodeStats()
				chunk = 0
			})
			mu.Unlock()
		}()

		return nil
	}, func() {})
	require.NoError(t, err)

	wgNodes.Wait()
	reportNodeStats()
	t.Logf("Done syncing %d nodes", total)

	total = 0
	chunk = 0
	t0 = time.Now()

	reportChanStats := func() {
		elapsed := time.Since(t0).Seconds()
		ratePerSec := float64(chunk) / elapsed
		t.Logf("Synced %d channels (and its "+
			"policies) (last chunk: %d) "+
			"(%.2f channels/second)",
			total, chunk, ratePerSec)

		t0 = time.Now()
	}

	var wgChans sync.WaitGroup
	err = src.ForEachChannel(ctx, func(info *models.ChannelEdgeInfo,
		policy1, policy2 *models.ChannelEdgePolicy) error {

		// Add each channel & policy. We do this in a goroutine to
		// take advantage of batch processing.
		wgChans.Add(1)
		go func() {
			defer wgChans.Done()

			err := dest.AddChannelEdge(
				ctx, info, batch.LazyAdd(),
			)
			if !errors.Is(err, ErrEdgeAlreadyExist) {
				require.NoError(t, err)
			}

			if policy1 != nil {
				err = dest.UpdateEdgePolicy(
					ctx, policy1, batch.LazyAdd(),
				)
				require.NoError(t, err)
			}

			if policy2 != nil {
				err = dest.UpdateEdgePolicy(
					ctx, policy2, batch.LazyAdd(),
				)
				require.NoError(t, err)
			}

			mu.Lock()
			total++
			chunk++
			s.Do(func() {
				reportChanStats()
				chunk = 0
			})
			mu.Unlock()
		}()

		return nil
	}, func() {})
	require.NoError(t, err)

	wgChans.Wait()
	reportChanStats()

	t.Logf("Done syncing %d channels", total)
}

// BenchmarkCacheLoading benchmarks how long it takes to load the in-memory
// graph cache from a populated database.
//
// NOTE: this is to be run against a local graph database. It can be run
// either against a kvdb-bbolt channel.db file, or a kvdb-sqlite channel.sqlite
// file or a postgres connection containing the channel graph in kvdb format and
// finally, it can be run against a native SQL sqlite or postgres database.
//
// NOTE: the TestPopulateDBs test helper can be used to populate a set of test
// DBs from a single source db.
func BenchmarkCacheLoading(b *testing.B) {
	ctx := context.Background()

	tests := []dbConnection{
		kvdbBBoltConn,
		kvdbSqliteConn,
		nativeSQLSqliteConn,
		kvdbPostgresConn,
		nativeSQLPostgresConn,
	}

	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			store := test.open(b)

			// Reset timer to exclude setup time.
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				graph, err := NewChannelGraph(store)
				require.NoError(b, err)
				b.StartTimer()

				require.NoError(b, graph.populateCache(ctx))
			}
		})
	}
}

// BenchmarkGraphReadMethods benchmarks various read calls of various V1Store
// implementations.
//
// NOTE: this is to be run against a local graph database. It can be run
// either against a kvdb-bbolt channel.db file, or a kvdb-sqlite channel.sqlite
// file or a postgres connection containing the channel graph in kvdb format and
// finally, it can be run against a native SQL sqlite or postgres database.
//
// NOTE: the TestPopulateDBs test helper can be used to populate a set of test
// DBs from a single source db.
func BenchmarkGraphReadMethods(b *testing.B) {
	ctx := context.Background()

	backends := []dbConnection{
		kvdbBBoltConn,
		kvdbSqliteConn,
		nativeSQLSqliteConn,
		kvdbPostgresConn,
		nativeSQLPostgresConn,
	}

	// We use a counter to make sure that any call-back is doing something
	// useful, otherwise the compiler may optimize it away in the future.
	var counter int64

	tests := []struct {
		name string
		fn   func(b testing.TB, store V1Store)
	}{
		{
			name: "ForEachNode",
			fn: func(b testing.TB, store V1Store) {
				err := store.ForEachNode(
					ctx,
					func(_ *models.LightningNode) error {
						// Increment the counter to
						// ensure the callback is doing
						// something.
						counter++

						return nil
					}, func() {},
				)
				require.NoError(b, err)
			},
		},
		{
			name: "ForEachChannel",
			fn: func(b testing.TB, store V1Store) {
				//nolint:ll
				err := store.ForEachChannel(
					ctx, func(_ *models.ChannelEdgeInfo,
						_ *models.ChannelEdgePolicy,
						_ *models.ChannelEdgePolicy) error {

						// Increment the counter to
						// ensure the callback is doing
						// something.
						counter++

						return nil
					}, func() {},
				)
				require.NoError(b, err)
			},
		},
		{
			name: "NodeUpdatesInHorizon",
			fn: func(b testing.TB, store V1Store) {
				_, err := store.NodeUpdatesInHorizon(
					time.Unix(0, 0), time.Now(),
				)
				require.NoError(b, err)
			},
		},
		{
			name: "ForEachNodeCacheable",
			fn: func(b testing.TB, store V1Store) {
				err := store.ForEachNodeCacheable(
					ctx, func(_ route.Vertex,
						_ *lnwire.FeatureVector) error {

						// Increment the counter to
						// ensure the callback is doing
						// something.
						counter++

						return nil
					}, func() {},
				)
				require.NoError(b, err)
			},
		},
		{
			name: "ForEachNodeCached",
			fn: func(b testing.TB, store V1Store) {
				//nolint:ll
				err := store.ForEachNodeCached(
					ctx, false, func(context.Context,
						route.Vertex,
						[]net.Addr,
						map[uint64]*DirectedChannel) error {

						// Increment the counter to
						// ensure the callback is doing
						// something.
						counter++

						return nil
					}, func() {},
				)
				require.NoError(b, err)
			},
		},
		{
			name: "ChanUpdatesInHorizon",
			fn: func(b testing.TB, store V1Store) {
				_, err := store.ChanUpdatesInHorizon(
					time.Unix(0, 0), time.Now(),
				)
				require.NoError(b, err)
			},
		},
	}

	for _, test := range tests {
		for _, db := range backends {
			name := fmt.Sprintf("%s-%s", test.name, db.name)
			b.Run(name, func(b *testing.B) {
				store := db.open(b)

				// Reset timer to exclude setup time.
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					test.fn(b, store)
				}
			})
		}
	}
}

// BenchmarkFindOptimalSQLQueryConfig uses the ForEachNode and ForEachChannel
// methods to find the optimal maximum sqldb QueryConfig values for a given
// database backend. This is useful for determining the best default values for
// each backend. The ForEachNode and ForEachChannel methods are used since
// they make use of both batching and pagination.
func BenchmarkFindOptimalSQLQueryConfig(b *testing.B) {
	// NOTE: Set this to true if you want to test with a postgres backend.
	testPostgres := false

	// NOTE: Set this to true if you want to test with various batch sizes.
	// By default, page sizes will be tested.
	testBatching := false

	// Set the various page sizes we want to test.
	//
	// NOTE: these are the sqlite paging testing values.
	testSizes := []uint32{20, 50, 100, 150, 500}

	configOption := "MaxPageSize"
	if testBatching {
		configOption = "MaxBatchSize"

		testSizes = []uint32{
			50, 100, 150, 200, 250, 300, 350,
		}
	}

	dbName := "sqlite"
	if testPostgres {
		dbName = "postgres"

		// Set the various page sizes we want to test.
		//
		// NOTE: these are the postgres paging values.
		testSizes = []uint32{5000, 7000, 10000, 12000}

		if testBatching {
			testSizes = []uint32{
				1000, 2000, 5000, 7000, 10000,
			}
		}
	}

	for _, size := range testSizes {
		b.Run(fmt.Sprintf("%s-%s-%d", configOption, dbName, size),
			func(b *testing.B) {
				ctx := context.Background()

				cfg := sqldb.DefaultSQLiteConfig()
				if testPostgres {
					cfg = sqldb.DefaultPostgresConfig()
				}

				if testBatching {
					cfg.MaxBatchSize = size
				} else {
					cfg.MaxPageSize = size
				}

				store := connectNativeSQLite(
					b, cfg, nativeSQLSqlitePath,
					nativeSQLSqliteFile,
				)
				if testPostgres {
					store = connectNativePostgres(
						b, cfg, nativeSQLPostgresDNS,
					)
				}

				// Reset timer to exclude setup time.
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					var (
						numNodes    = 0
						numChannels = 0
					)

					//nolint:ll
					err := store.ForEachNode(
						ctx,
						func(_ *models.LightningNode) error {
							numNodes++

							return nil
						}, func() {},
					)
					require.NoError(b, err)

					//nolint:ll
					err = store.ForEachChannel(
						ctx,
						func(_ *models.ChannelEdgeInfo,
							_,
							_ *models.ChannelEdgePolicy) error {

							numChannels++

							return nil
						}, func() {},
					)
					require.NoError(b, err)
				}
			},
		)
	}
}
