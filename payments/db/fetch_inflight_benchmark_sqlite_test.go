//go:build test_db_sqlite && !test_db_postgres

package paymentsdb

import (
	"database/sql"
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// sqliteFetchInFlightBenchBackend creates SQLite-backed benchmark stores.
type sqliteFetchInFlightBenchBackend struct{}

// newFetchInFlightBenchBackend creates the benchmark backend selected by the
// active build tags.
func newFetchInFlightBenchBackend(_ *testing.B) fetchInFlightBenchBackend {
	return sqliteFetchInFlightBenchBackend{}
}

// newStore creates a migrated SQLite SQLStore and seeds it with one benchmark
// scenario.
func (sqliteFetchInFlightBenchBackend) newStore(b *testing.B,
	scenario fetchInFlightBenchScenario,
	totalPayments int) (*SQLStore, fetchInFlightBenchSeedStats) {

	b.Helper()

	sqliteDB := sqldb.NewTestSqliteDB(b)
	baseDB := sqliteDB.BaseDB

	withTx := func(tx *sql.Tx) SQLQueries {
		return baseDB.WithTx(tx)
	}
	queries := sqldb.NewTransactionExecutor(baseDB, withTx)

	store, err := NewSQLStore(&SQLStoreConfig{
		QueryCfg: sqldb.DefaultSQLiteConfig(),
	}, queries)
	if err != nil {
		b.Fatalf("new SQL store: %v", err)
	}

	stats := seedFetchInFlightBenchDB(
		b, baseDB.DB, withTx, scenario, totalPayments,
	)

	return store, stats
}
