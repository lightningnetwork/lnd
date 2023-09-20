//go:build kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqlbase

import (
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// parseSqliteError attempts to parse a sqlite error as a database agnostic
// SQL error.
func parseSqliteError(err error) error {
	if err == nil {
		return nil
	}

	// If the error isn't wrapped properly, the errors.As call with fail,
	// so we'll also try to check the expected error message directly.
	// This is taken from:
	// https://gitlab.com/cznic/sqlite/-/blob/v1.25.0/sqlite.go#L75.
	const sqliteErrMsg = "SQLITE_BUSY"
	if strings.Contains(err.Error(), sqliteErrMsg) {
		return &ErrSerializationError{
			DBError: err,
		}
	}

	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return nil
	}

	switch sqliteErr.Code() {
	// Handle unique constraint violation error.
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return &ErrSQLUniqueConstraintViolation{
			DBError: sqliteErr,
		}

	// Database is currently busy, so we'll need to try again.
	case sqlite3.SQLITE_BUSY:
		return &ErrSerializationError{
			DBError: sqliteErr,
		}

	default:
		return fmt.Errorf("unknown sqlite error: %w", sqliteErr)
	}
}
