//go:build !kvdb_postgres
// +build !kvdb_postgres

package kvdb

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb/postgres"
)

const PostgresBackend = false

func NewPostgresFixture(t testing.TB, _ string) postgres.Fixture {
	t.Fatalf("postgres backend not available")
	return nil
}

func StartEmbeddedPostgres() (func() error, error) {
	return nil, errors.New("postgres backend not available")
}
