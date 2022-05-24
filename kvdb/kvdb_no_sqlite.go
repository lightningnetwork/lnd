//go:build !kvdb_sqlite
// +build !kvdb_sqlite

package kvdb

import (
	"errors"

	"github.com/lightningnetwork/lnd/kvdb/sqlite"
)

func NewSqliteFixture() (sqlite.Fixture, error) {
	return nil, errors.New("sqlite backend not available")
}
