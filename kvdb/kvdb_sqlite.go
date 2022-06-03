//go:build kvdb_sqlite
// +build kvdb_sqlite

package kvdb

import "github.com/lightningnetwork/lnd/kvdb/sqlite"

const TestBackend = SqliteBackendName

func NewSqliteFixture() (sqlite.Fixture, error) {
	f, err := sqlite.NewSqliteTestFixture("test")

	return f, err
}
