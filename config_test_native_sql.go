//go:build test_native_sql

package lnd

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
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

	return graphdb.NewSQLStore(
		&graphdb.SQLStoreConfig{
			ChainHash:     *d.cfg.ActiveNetParams.GenesisHash,
			PaginationCfg: sqldb.DefaultPagedQueryConfig(),
		},
		graphExecutor, opts...,
	)
}

// graphSQLMigration is the version number for the graph migration
// that migrates the KV graph to the native SQL schema.
const graphSQLMigration = 9

// getSQLMigration returns a migration function for the given version.
func getSQLMigration(ctx context.Context, version int,
	kvBackend kvdb.Backend,
	chain chainhash.Hash) (func(tx *sqlc.Queries) error, bool) {

	switch version {
	case graphSQLMigration:
		return func(tx *sqlc.Queries) error {
			err := graphdb.MigrateGraphToSQL(
				ctx, kvBackend, tx, chain,
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
