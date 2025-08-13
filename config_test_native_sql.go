//go:build test_native_sql

package lnd

import (
	"context"
	"database/sql"
	"fmt"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// RunTestSQLMigration is a build tag that indicates whether the test_native_sql
// build tag is set.
var RunTestSQLMigration = true

// getGraphStore returns a graphdb.V1Store backed by a graphdb.SQLStore
// implementation.
func (d *DefaultDatabaseBuilder) getGraphStore(baseDB *sqldb.BaseDB,
	_ kvdb.Backend, opts ...graphdb.StoreOptionModifier) (graphdb.V1Store,
	error) {

	graphExecutor := sqldb.NewTransactionExecutor(
		baseDB, func(tx *sql.Tx) graphdb.SQLQueries {
			return baseDB.WithTx(tx)
		},
	)

	queryConfig := d.cfg.DB.Sqlite.QueryConfig
	if d.cfg.DB.Backend == lncfg.PostgresBackend {
		queryConfig = d.cfg.DB.Postgres.QueryConfig
	}

	return graphdb.NewSQLStore(
		&graphdb.SQLStoreConfig{
			ChainHash: *d.cfg.ActiveNetParams.GenesisHash,
			QueryCfg:  &queryConfig,
		},
		graphExecutor, opts...,
	)
}

// graphSQLMigration is the version number for the graph migration
// that migrates the KV graph to the native SQL schema.
const graphSQLMigration = 10

// getSQLMigration returns a migration function for the given version.
func (d *DefaultDatabaseBuilder) getSQLMigration(ctx context.Context,
	version int, kvBackend kvdb.Backend) (func(tx *sqlc.Queries) error,
	bool) {

	cfg := &graphdb.SQLStoreConfig{
		ChainHash: *d.cfg.ActiveNetParams.GenesisHash,
		QueryCfg:  &d.cfg.DB.Sqlite.QueryConfig,
	}
	if d.cfg.DB.Backend == lncfg.PostgresBackend {
		cfg.QueryCfg = &d.cfg.DB.Postgres.QueryConfig
	}

	switch version {
	case graphSQLMigration:
		return func(tx *sqlc.Queries) error {
			err := graphdb.MigrateGraphToSQL(
				ctx, cfg, kvBackend, tx,
			)
			if err != nil {
				return fmt.Errorf("failed to migrate graph "+
					"to SQL: %w", err)
			}

			return nil
		}, true
	}

	// No version was matched, so we return false to indicate that no
	// migration is known for the given version.
	return nil, false
}
