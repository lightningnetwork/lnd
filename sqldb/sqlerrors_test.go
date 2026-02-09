//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestSqliteErrorCodeConstants verifies that the SQLite library constants used
// in parseSqliteError's serialization error classification have the expected
// integer values. This guards against silent changes in the upstream sqlite
// library that could break our error code switch statements. Note that we
// cannot test parseSqliteError directly because sqlite.Error has unexported
// fields that prevent constructing test instances.
func TestSqliteErrorCodeConstants(t *testing.T) {
	t.Parallel()

	// These are the error codes that parseSqliteError classifies as
	// serialization errors eligible for transaction retry.
	serializableCodes := []struct {
		name string
		code int
	}{
		{
			name: "SQLITE_BUSY",
			code: sqlite3.SQLITE_BUSY,
		},
		{
			name: "SQLITE_IOERR",
			code: sqlite3.SQLITE_IOERR,
		},
		{
			name: "SQLITE_FULL",
			code: sqlite3.SQLITE_FULL,
		},
		{
			name: "SQLITE_LOCKED",
			code: sqlite3.SQLITE_LOCKED,
		},
	}

	for _, tc := range serializableCodes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Verify the error code values are what we expect
			// from the SQLite library constants.
			switch tc.code {
			case sqlite3.SQLITE_BUSY:
				require.Equal(t, 5, tc.code)

			case sqlite3.SQLITE_IOERR:
				require.Equal(t, 10, tc.code)

			case sqlite3.SQLITE_FULL:
				require.Equal(t, 13, tc.code)

			case sqlite3.SQLITE_LOCKED:
				require.Equal(t, 6, tc.code)
			}
		})
	}
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
			name:            "SQLITE_BUSY string match",
			err:             errors.New("SQLITE_BUSY: database table is locked"),
			isSerialization: true,
		},
		{
			name:            "unrelated error is not serialization",
			err:             errors.New("some other database error"),
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
			name: "postgres deadlock string match",
			err:  errors.New("deadlock detected"),

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
