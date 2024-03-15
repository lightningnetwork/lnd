//go:build kvdb_postgres
// +build kvdb_postgres

package kvdb

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb/postgres"
)

const PostgresBackend = true

func NewPostgresFixture(t testing.TB, dbName string) (postgres.Fixture, error) {
	return postgres.NewFixture(t, dbName)
}

func StartEmbeddedPostgres() (func() error, error) {
	return postgres.StartEmbeddedPostgres()
}
