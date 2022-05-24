//go:build !kvdb_sqlite
// +build !kvdb_sqlite

package kvdb

import (
	"errors"
)

const SqliteBackend = false

func NewSqliteFixture(dbName string) (string, error) {
	return "", errors.New("sqlite backend not available")
}
