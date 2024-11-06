//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrRetriesExceeded is returned when a transaction is retried more
	// than the max allowed valued without a success.
	ErrRetriesExceeded = errors.New("db tx retries exceeded")

	// postgresErrMsgs are strings that signify retriable errors resulting
	// from serialization failures.
	postgresErrMsgs = []string{
		"could not serialize access",
		"current transaction is aborted",
		"not enough elements in RWConflictPool",
		"deadlock detected",
		"commit unexpectedly resulted in rollback",
	}
)

// MapSQLError attempts to interpret a given error as a database agnostic SQL
// error.
func MapSQLError(err error) error {
	if err == nil {
		return nil
	}

	// Attempt to interpret the error as a sqlite error.
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return parseSqliteError(sqliteErr)
	}

	// Attempt to interpret the error as a postgres error.
	var pqErr *pgconn.PgError
	if errors.As(err, &pqErr) {
		return parsePostgresError(pqErr)
	}

	// Sometimes the error won't be properly wrapped, so we'll need to
	// inspect raw error itself to detect something we can wrap properly.
	// This handles a postgres variant of the error.
	for _, postgresErrMsg := range postgresErrMsgs {
		if strings.Contains(err.Error(), postgresErrMsg) {
			return &ErrSerializationError{
				DBError: err,
			}
		}
	}

	// We'll also attempt to catch this for sqlite, that uses a slightly
	// different error message. This is taken from:
	// https://gitlab.com/cznic/sqlite/-/blob/v1.25.0/sqlite.go#L75.
	const sqliteErrMsg = "SQLITE_BUSY"
	if strings.Contains(err.Error(), sqliteErrMsg) {
		return &ErrSerializationError{
			DBError: err,
		}
	}

	// Return original error if it could not be classified as a database
	// specific error.
	return err
}

// parsePostgresError attempts to parse a sqlite error as a database agnostic
// SQL error.
func parseSqliteError(sqliteErr *sqlite.Error) error {
	switch sqliteErr.Code() {
	// Handle unique constraint violation error.
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return &ErrSQLUniqueConstraintViolation{
			DBError: sqliteErr,
		}

	case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
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

// parsePostgresError attempts to parse a postgres error as a database agnostic
// SQL error.
func parsePostgresError(pqErr *pgconn.PgError) error {
	switch pqErr.Code {
	// Handle unique constraint violation error.
	case pgerrcode.UniqueViolation:
		return &ErrSQLUniqueConstraintViolation{
			DBError: pqErr,
		}

	// Unable to serialize the transaction, so we'll need to try again.
	case pgerrcode.SerializationFailure:
		return &ErrSerializationError{
			DBError: pqErr,
		}

	// In failed SQL transaction because we didn't catch a previous
	// serialization error, so return this one as a serialization error.
	case pgerrcode.InFailedSQLTransaction:
		return &ErrSerializationError{
			DBError: pqErr,
		}

	// Deadlock detedted because of a serialization error, so return this
	// one as a serialization error.
	case pgerrcode.DeadlockDetected:
		return &ErrSerializationError{
			DBError: pqErr,
		}

	default:
		return fmt.Errorf("unknown postgres error: %w", pqErr)
	}
}

// ErrSQLUniqueConstraintViolation is an error type which represents a database
// agnostic SQL unique constraint violation.
type ErrSQLUniqueConstraintViolation struct {
	DBError error
}

func (e ErrSQLUniqueConstraintViolation) Error() string {
	return fmt.Sprintf("sql unique constraint violation: %v", e.DBError)
}

// ErrSerializationError is an error type which represents a database agnostic
// error that a transaction couldn't be serialized with other concurrent db
// transactions.
type ErrSerializationError struct {
	DBError error
}

// Unwrap returns the wrapped error.
func (e ErrSerializationError) Unwrap() error {
	return e.DBError
}

// Error returns the error message.
func (e ErrSerializationError) Error() string {
	return e.DBError.Error()
}

// IsSerializationError returns true if the given error is a serialization
// error.
func IsSerializationError(err error) bool {
	var serializationError *ErrSerializationError
	return errors.As(err, &serializationError)
}
