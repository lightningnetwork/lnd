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
// been excluded from the build. All Postgres-requiring helpers return early
// via t.Skip so any test path that actually exercises a Postgres backend is
// skipped rather than panicking.
//
// The build tag on this file is the exact inverse of the tag on
// postgres_fixture.go so that exactly one definition of TestPgFixture is
// compiled for any given (tag, GOOS, GOARCH) tuple.
type TestPgFixture struct{}

// NewTestPgFixture returns an empty stub fixture. Constructing it is a
// no-op; only callers that go on to allocate a real Postgres database via
// NewTestPostgresDB / NewTestPostgresDBWithVersion will be skipped. This
// lets parametrized tests that have both a SQLite and a Postgres branch
// continue running their SQLite branch unaffected.
func NewTestPgFixture(_ testing.TB, _ time.Duration) *TestPgFixture {
	return &TestPgFixture{}
}

// GetConfig returns an empty config for the stub fixture. Tests that reach
// here should already have been skipped by NewTestPostgresDB; this exists
// purely so the method set matches the real fixture.
func (f *TestPgFixture) GetConfig(_ string) *PostgresConfig {
	return &PostgresConfig{}
}

// TearDown is a no-op for the stub fixture.
func (f *TestPgFixture) TearDown(_ testing.TB) {}

// NewTestPostgresDB skips any test that reaches it; the real implementation
// is only available when the build tag `test_db_postgres` is set, which
// also pulls in the dockertest-based fixture.
func NewTestPostgresDB(t testing.TB, _ *TestPgFixture) *PostgresStore {
	t.Skip("postgres test fixture not compiled in; rerun with " +
		"-tags=test_db_postgres")
	return nil
}

// NewTestPostgresDBWithVersion is the version-pinned variant of
// NewTestPostgresDB and behaves identically as a skip stub.
func NewTestPostgresDBWithVersion(t *testing.T, _ *TestPgFixture,
	_ uint) *PostgresStore {

	t.Skip("postgres test fixture not compiled in; rerun with " +
		"-tags=test_db_postgres")
	return nil
}
