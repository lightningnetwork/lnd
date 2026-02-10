//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestSqliteErrBaseCode verifies that sqliteErrBaseCode correctly extracts the
// primary error code from SQLite extended error codes by masking the lower 8
// bits.
func TestSqliteErrBaseCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		code     int
		baseCode int
	}{
		{
			// Base code returns itself.
			name:     "SQLITE_IOERR base",
			code:     sqlite3.SQLITE_IOERR,
			baseCode: sqlite3.SQLITE_IOERR,
		},
		{
			// SQLITE_IOERR_READ = SQLITE_IOERR | (1<<8) = 266.
			name:     "SQLITE_IOERR_READ extended",
			code:     266,
			baseCode: sqlite3.SQLITE_IOERR,
		},
		{
			// SQLITE_IOERR_WRITE = SQLITE_IOERR | (3<<8) = 778.
			name:     "SQLITE_IOERR_WRITE extended",
			code:     778,
			baseCode: sqlite3.SQLITE_IOERR,
		},
		{
			// SQLITE_IOERR_GETTEMPPATH = 6410, the code from
			// the original Android bug report.
			name:     "SQLITE_IOERR_GETTEMPPATH extended (6410)",
			code:     6410,
			baseCode: sqlite3.SQLITE_IOERR,
		},
		{
			// SQLITE_BUSY_SNAPSHOT = SQLITE_BUSY | (2<<8) = 517.
			name:     "SQLITE_BUSY_SNAPSHOT extended",
			code:     517,
			baseCode: sqlite3.SQLITE_BUSY,
		},
		{
			// SQLITE_LOCKED_SHAREDCACHE = SQLITE_LOCKED |
			// (1<<8) = 262.
			name:     "SQLITE_LOCKED_SHAREDCACHE extended",
			code:     262,
			baseCode: sqlite3.SQLITE_LOCKED,
		},
		{
			// Base code returns itself.
			name:     "SQLITE_FULL base",
			code:     sqlite3.SQLITE_FULL,
			baseCode: sqlite3.SQLITE_FULL,
		},
		{
			// Constraint violations use exact extended codes,
			// but the base extraction should still work.
			name:     "SQLITE_CONSTRAINT_UNIQUE base extraction",
			code:     sqlite3.SQLITE_CONSTRAINT_UNIQUE,
			baseCode: 19, // SQLITE_CONSTRAINT
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, tc.baseCode, sqliteErrBaseCode(tc.code),
			)
		})
	}
}

// TestSqliteErrorCodeConstants verifies that the SQLite library constants used
// in parseSqliteError have the expected integer values. This guards against
// silent changes in the upstream sqlite library that could break our error
// classification.
func TestSqliteErrorCodeConstants(t *testing.T) {
	t.Parallel()

	// Verify base error codes used for retryable classification.
	require.Equal(t, 5, sqlite3.SQLITE_BUSY)
	require.Equal(t, 10, sqlite3.SQLITE_IOERR)
	require.Equal(t, 13, sqlite3.SQLITE_FULL)
	require.Equal(t, 6, sqlite3.SQLITE_LOCKED)

	// Verify constraint violation extended codes.
	require.Equal(t, 2067, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	require.Equal(t, 1555, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)

	// Verify that known extended codes have the expected base code.
	// This ensures that our base code masking approach is correct.
	require.Equal(
		t, sqlite3.SQLITE_IOERR,
		sqliteErrBaseCode(sqlite3.SQLITE_IOERR_WRITE),
	)
	require.Equal(
		t, sqlite3.SQLITE_BUSY,
		sqliteErrBaseCode(sqlite3.SQLITE_BUSY_SNAPSHOT),
	)
	require.Equal(
		t, sqlite3.SQLITE_LOCKED,
		sqliteErrBaseCode(sqlite3.SQLITE_LOCKED_SHAREDCACHE),
	)
}

// TestMapSQLErrorStringFallback verifies that the string-based error detection
// works for errors that don't wrap sqlite.Error or pgconn.PgError directly.
func TestMapSQLErrorStringFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		err             error
		isSerialization bool
	}{
		{
			name: "SQLITE_BUSY string match",
			err: errors.New(
				"SQLITE_BUSY: database table is locked",
			),
			isSerialization: true,
		},
		{
			name: "unrelated error is not serialization",
			err:  errors.New("some other database error"),
			isSerialization: false,
		},
		{
			name:            "nil error",
			err:             nil,
			isSerialization: false,
		},
		{
			name: "wrapped generic error passes through",
			err: fmt.Errorf(
				"operation failed: %w",
				errors.New("timeout"),
			),
			isSerialization: false,
		},
		{
			name: "postgres serialization string match",
			err: errors.New(
				"could not serialize access",
			),
			isSerialization: true,
		},
		{
			name:            "postgres deadlock string match",
			err:             errors.New("deadlock detected"),
			isSerialization: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mapped := MapSQLError(tc.err)
			result := IsSerializationError(mapped)
			require.Equal(t, tc.isSerialization, result)
		})
	}
}
