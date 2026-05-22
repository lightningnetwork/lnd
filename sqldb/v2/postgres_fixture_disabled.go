//go:build !test_db_postgres || js || (windows && (arm || 386)) || (linux && (ppc64 || mips || mipsle || mips64)) || netbsd || openbsd

package sqldb

import (
	"testing"
	"time"
)

// PostgresTag is exposed as a no-op constant when the postgres test fixture
// is not compiled in.
const PostgresTag = ""

// TestPgFixture is a stub used when the real dockertest-based fixture has
// been excluded from the build. See sqldb/postgres_fixture_disabled.go for
// the rationale.
type TestPgFixture struct{}

// NewTestPgFixture returns an empty stub fixture; see the v1 sibling file
// for design notes.
func NewTestPgFixture(_ testing.TB, _ time.Duration) *TestPgFixture {
	return &TestPgFixture{}
}

// GetConfig returns an empty config for the stub fixture.
func (f *TestPgFixture) GetConfig(_ string) *PostgresConfig {
	return &PostgresConfig{}
}

// TearDown is a no-op for the stub fixture.
func (f *TestPgFixture) TearDown(_ testing.TB) {}

// RandomDBName returns a constant identifier when the real fixture is not
// compiled in. Tests that get this far should already have been skipped.
func RandomDBName(_ testing.TB) string {
	return "test_disabled"
}

// NewTestPostgresDB skips the calling test; see the v1 sibling.
func NewTestPostgresDB(t testing.TB, _ *TestPgFixture,
	_ []MigrationSet) *PostgresStore {

	t.Skip("postgres test fixture not compiled in; rerun with " +
		"-tags=test_db_postgres")
	return nil
}

// NewTestPostgresDBWithVersion skips the calling test; see the v1 sibling.
func NewTestPostgresDBWithVersion(t testing.TB, _ *TestPgFixture,
	_ MigrationSet, _ uint) *PostgresStore {

	t.Skip("postgres test fixture not compiled in; rerun with " +
		"-tags=test_db_postgres")
	return nil
}
