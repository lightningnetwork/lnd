//go:build test_native_sql

package lnd

import (
	"database/sql"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb"
)

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
			ChainHash: *d.cfg.ActiveNetParams.GenesisHash,
		},
		graphExecutor, opts...,
	)
}
