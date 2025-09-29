//go:build test_native_sql

package lnd

import (
	"context"
	"database/sql"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// RunTestSQLMigration is a build tag that indicates whether the test_native_sql
// build tag is set.
var RunTestSQLMigration = true

// getSQLMigration returns a migration function for the given version.
func (d *DefaultDatabaseBuilder) getSQLMigration(_ context.Context,
	version int, _ kvdb.Backend) (func(tx *sqlc.Queries) error,
	bool) {

	switch version {
	default:
		// No version was matched, so we return false to indicate that
		// no migration is known for the given version.
		return nil, false
	}
}

// getPaymentsStore returns a paymentsdb.DB backed by a paymentsdb.SQLStore
// implementation.
func (d *DefaultDatabaseBuilder) getPaymentsStore(baseDB *sqldb.BaseDB,
	kvBackend kvdb.Backend,
	opts ...paymentsdb.OptionModifier) (paymentsdb.DB, error) {

	paymentsExecutor := sqldb.NewTransactionExecutor(
		baseDB, func(tx *sql.Tx) paymentsdb.SQLQueries {
			return baseDB.WithTx(tx)
		},
	)

	queryConfig := d.cfg.DB.Sqlite.QueryConfig
	if d.cfg.DB.Backend == lncfg.PostgresBackend {
		queryConfig = d.cfg.DB.Postgres.QueryConfig
	}

	return paymentsdb.NewSQLStore(
		&paymentsdb.SQLStoreConfig{
			QueryCfg: &queryConfig,
		},
		paymentsExecutor, opts...,
	)
}
