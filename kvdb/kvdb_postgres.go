//go:build kvdb_postgres
// +build kvdb_postgres

package kvdb

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb/postgres"
)

const PostgresBackend = true

func NewPostgresFixture(t testing.TB, dbName string) postgres.Fixture {
	return postgres.NewTestPgFixture(t, dbName)
}

func StartEmbeddedPostgres() (func() error, error) {
	return postgres.StartEmbeddedPostgres()
}
