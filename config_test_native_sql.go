//go:build test_native_sql

package lnd

import (
	"context"
	"database/sql"
	"fmt"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

const graphSQLMigration = 9

func getSQLMigration(ctx context.Context, version int,
	kvGraphStore *graphdb.KVStore) (func(tx *sqlc.Queries) error, bool) {

	if version != graphSQLMigration {
		return nil, false
	}

	return func(tx *sqlc.Queries) error {
		err := graphdb.MigrateGraphToSQL(
			ctx, kvGraphStore, tx,
		)
		if err != nil {
			return fmt.Errorf("failed to migrate "+
				"graph to SQL: %w", err)
		}

		return nil
	}, true
}

func (d *DefaultDatabaseBuilder) getGraphStore(baseDB *sqldb.BaseDB,
	_ *graphdb.KVStore,
	opts ...graphdb.StoreOptionModifier) (graphdb.V1Store, error) {

	graphExecutor := sqldb.NewTransactionExecutor(
		baseDB, func(tx *sql.Tx) graphdb.SQLQueries {
			return baseDB.WithTx(tx)
		},
	)

	return graphdb.NewSQLStore(
		&graphdb.SQLStoreConfig{
			ChainHash: *d.cfg.ActiveNetParams.GenesisHash,
		},
		graphExecutor, opts...,
	)
}
