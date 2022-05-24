//go:build kvdb_sqlite
// +build kvdb_sqlite

package kvdb

const SqliteBackend = true

func NewSqliteFixture(dbName string) (string, error) {
	return "", nil
}
