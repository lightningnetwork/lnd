//go:build !kvdb_postgres
// +build !kvdb_postgres

package kvdb

import (
	"errors"

	"github.com/lightningnetwork/lnd/kvdb/postgres"
)

const PostgresBackend = false

func NewPostgresFixture(dbName string) (postgres.Fixture, error) {
	return nil, errors.New("postgres backend not available")
}

func StartEmbeddedPostgres() (func() error, error) {
	return nil, errors.New("postgres backend not available")
}
