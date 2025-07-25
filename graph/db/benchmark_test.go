package graphdb

import (
	"context"
	"database/sql"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"path"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
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

	// testSqlPaginationCfg is used to configure the pagination settings for
	// the SQL stores we open for testing.
	testSqlPaginationCfg = sqldb.DefaultPagedQueryConfig()

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
				b, nativeSQLSqlitePath, nativeSQLSqliteFile,
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
			return connectNativePostgres(b, nativeSQLPostgresDNS)
		},
	}
)

// connectNativePostgres creates a V1Store instance backed by a native Postgres
// database for testing purposes.
func connectNativePostgres(t testing.TB, dsn string) V1Store {
	store, err := sqldb.NewPostgresStore(&sqldb.PostgresConfig{
		Dsn:            dsn,
		MaxConnections: testMaxPostgresConnections,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	return newSQLStore(t, store)
}

// connectNativeSQLite creates a V1Store instance backed by a native SQLite
// database for testing purposes.
func connectNativeSQLite(t testing.TB, dbPath, file string) V1Store {
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

	return newSQLStore(t, store)
}

// connectKVDBPostgres creates a V1Store instance backed by a kvdb-postgres
// database for testing purposes.
func connectKVDBPostgres(t testing.TB, dsn string) V1Store {
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

	return newKVStore(t, kvStore)
}

// connectKVDBSqlite creates a V1Store instance backed by a kvdb-sqlite
// database for testing purposes.
func connectKVDBSqlite(t testing.TB, dbPath, fileName string) V1Store {
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

	return newKVStore(t, kvStore)
}

// connectBBoltDB creates a new BBolt database connection for testing.
func connectBBoltDB(t testing.TB, dbPath, fileName string) V1Store {
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

	return newKVStore(t, kvStore)
}

// newKVStore creates a new KVStore instance for testing using a provided
// kvdb.Backend instance.
func newKVStore(t testing.TB, backend kvdb.Backend) V1Store {
	store, err := NewKVStore(backend, testStoreOptions...)
	require.NoError(t, err)

	return store
}

// newSQLStore creates a new SQLStore instance for testing using a provided
// sqldb.DB instance.
func newSQLStore(t testing.TB, db sqldb.DB) V1Store {
	err := db.ApplyAllMigrations(
		context.Background(), sqldb.GetMigrations(),
	)
	require.NoError(t, err)

	graphExecutor := sqldb.NewTransactionExecutor(
		db.GetBaseDB(), func(tx *sql.Tx) SQLQueries {
			return db.GetBaseDB().WithTx(tx)
		},
	)

	store, err := NewSQLStore(
		&SQLStoreConfig{
			ChainHash:     dbTestChain,
			PaginationCfg: testSqlPaginationCfg,
		},
		graphExecutor, testStoreOptions...,
	)
	require.NoError(t, err)

	return store
}
