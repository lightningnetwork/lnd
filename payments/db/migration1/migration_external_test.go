//go:build test_db_postgres || test_db_sqlite

package migration1

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestMigrationWithExternalDB tests the migration of the payment store from a
// bolt backed channel.db or a kvdb channel.sqlite to a SQL database. Note that
// this test does not attempt to be a complete migration test for all payment
// store types but rather is added as a tool for developers and users to debug
// payment migration issues with an actual channel.db/channel.sqlite file.
//
// NOTE: To use this test, place either of those files in the
// payments/db/migration1/testdata directory, uncomment the "Skipf" line, and
// set the "fileName" variable to the name of the channel database file you
// want to use for the migration test.
func TestMigrationWithExternalDB(t *testing.T) {
	ctx := context.Background()

	// NOTE: comment this line out to run the test.
	t.Skipf("skipping test meant for local debugging only")

	// NOTE: set this to the name of the channel database file you want
	// to use for the migration test. This may be either a bbolt ".db" file
	// or a SQLite ".sqlite" file. If you want to migrate from a
	// bbolt channel.db file, set this to "channel.db".
	const fileName = "backup_channeldb.sqlite"

	// NOTE: if set, this test will prefer migrating from a Postgres-backed
	// kvdb source instead of a local file. Leave empty to use fileName.
	const postgresKVDSN = ""
	const postgresKVPfx = "channeldb"
	const logSequenceOrder = false

	// Determine if we are using a SQLite file or a Bolt DB file.
	isSqlite := strings.HasSuffix(fileName, ".sqlite")

	// Set up logging for the test.
	logger := btclog.NewSLogger(btclog.NewDefaultHandler(os.Stdout))
	UseLogger(logger)

	// migrate runs the migration from the kvdb store to the SQL store.
	migrate := func(t *testing.T, kvBackend kvdb.Backend) {
		sqlStore := setupTestSQLDB(t)

		// Run migration in a transaction
		err := sqlStore.db.ExecTx(
			ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
				return MigratePaymentsKVToSQL(
					ctx, kvBackend, tx, &SQLStoreConfig{
						QueryCfg: sqlStore.cfg.QueryCfg,
					},
				)
			}, sqldb.NoOpReset,
		)
		require.NoError(t, err)

		_ = logSequenceOrder
	}

	connectPostgres := func(t *testing.T, dsn, prefix string) kvdb.Backend {
		dsn = strings.TrimSpace(dsn)
		if dsn == "" {
			t.Fatalf("missing postgres kvdb dsn")
		}

		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			prefix = "channeldb"
		}

		const (
			timeout  = 10 * time.Second
			maxConns = 5
		)
		sqlbase.Init(maxConns)

		dbCfg := &postgres.Config{
			Dsn:            dsn,
			Timeout:        timeout,
			MaxConnections: maxConns,
		}

		kvStore, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx, dbCfg, prefix,
		)
		require.NoError(t, err)

		return kvStore
	}

	connectPostgresKV := func(t *testing.T) kvdb.Backend {
		return connectPostgres(t, postgresKVDSN, postgresKVPfx)
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
			maxConns = 5
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
			name:   "testdata",
			dbPath: "testdata",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if postgresKVDSN != "" {
				migrate(t, connectPostgresKV(t))
				return
			}

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
