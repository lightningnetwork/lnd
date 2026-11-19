//go:build !test_db_postgres

package sqldb

import (
	"database/sql"
	"testing"
	"time"
)

const PostgresTag = "15"

// TestPgFixture is only available when built with the test_db_postgres tag.
type TestPgFixture struct{}

// NewTestPgFixture constructs a new Postgres fixture when test_db_postgres is
// enabled.
func NewTestPgFixture(t testing.TB, _ time.Duration) *TestPgFixture {
	postgresFixtureUnavailable(t)
	return nil
}

// GetConfig returns the full config of the Postgres node.
func (*TestPgFixture) GetConfig(string) *PostgresConfig {
	panic("Postgres test fixture requires the test_db_postgres build tag")
}

// TearDown stops the underlying docker container.
func (*TestPgFixture) TearDown(t testing.TB) {
	t.Helper()
}

// DB returns the fixture database.
func (*TestPgFixture) DB() *sql.DB {
	panic("Postgres test fixture requires the test_db_postgres build tag")
}

// RandomDBName generates a random database name.
func RandomDBName(t testing.TB) string {
	postgresFixtureUnavailable(t)
	return ""
}

// NewTestPostgresDB is a helper function that creates a Postgres database for
// testing using the given fixture.
func NewTestPostgresDB(t testing.TB, _ *TestPgFixture,
	_ []MigrationSet) *PostgresStore {

	postgresFixtureUnavailable(t)
	return nil
}

// NewTestPostgresDBWithVersion is a helper function that creates a Postgres
// database for testing and migrates it to the given version.
func NewTestPostgresDBWithVersion(t testing.TB, _ *TestPgFixture,
	_ MigrationSet, _ uint) *PostgresStore {

	postgresFixtureUnavailable(t)
	return nil
}

func postgresFixtureUnavailable(t testing.TB) {
	t.Helper()
	t.Skip("Postgres test fixture requires the test_db_postgres build tag")
}
