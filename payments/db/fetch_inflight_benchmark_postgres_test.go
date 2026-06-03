//go:build test_db_postgres && !test_db_sqlite

package paymentsdb

import (
	"database/sql"
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// postgresFetchInFlightBenchBackend creates Postgres-backed benchmark stores.
type postgresFetchInFlightBenchBackend struct {
	fixture *sqldb.TestPgFixture
}

// newFetchInFlightBenchBackend creates the benchmark backend selected by the
// active build tags.
func newFetchInFlightBenchBackend(b *testing.B) fetchInFlightBenchBackend {
	b.Helper()

	fixture := sqldb.NewTestPgFixture(
		b, sqldb.DefaultPostgresFixtureLifetime,
	)
	b.Cleanup(func() {
		fixture.TearDown(b)
	})

	return postgresFetchInFlightBenchBackend{fixture: fixture}
}

// newStore creates a migrated Postgres SQLStore and seeds it with one benchmark
// scenario.
func (backend postgresFetchInFlightBenchBackend) newStore(b *testing.B,
	scenario fetchInFlightBenchScenario,
	totalPayments int) (*SQLStore, fetchInFlightBenchSeedStats) {

	b.Helper()

	pgDB := sqldb.NewTestPostgresDB(b, backend.fixture)
	baseDB := pgDB.BaseDB

	withTx := func(tx *sql.Tx) SQLQueries {
		return baseDB.WithTx(tx)
	}
	queries := sqldb.NewTransactionExecutor(baseDB, withTx)

	store, err := NewSQLStore(&SQLStoreConfig{
		QueryCfg: sqldb.DefaultPostgresConfig(),
	}, queries)
	if err != nil {
		b.Fatalf("new SQL store: %v", err)
	}

	stats := seedFetchInFlightBenchDB(
		b, baseDB.DB, withTx, scenario, totalPayments,
	)

	return store, stats
}
