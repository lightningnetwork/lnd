//go:build js || (windows && (arm || 386)) || (linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"errors"
	"fmt"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
)

var (
	// ErrTxRetriesExceeded is returned when a transaction is retried more
	// than the max allowed valued without a success.
	ErrTxRetriesExceeded = errors.New("db tx retries exceeded")
)

// MapSQLError attempts to interpret a given error as a database agnostic SQL
// error.
func MapSQLError(err error) error {
	// Attempt to interpret the error as a postgres error.
	var pqErr *pgconn.PgError
	if errors.As(err, &pqErr) {
		return parsePostgresError(pqErr)
	}

	// Return original error if it could not be classified as a database
	// specific error.
	return err
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

	// Deadlock detected because of a serialization error, so return this
	// one as a serialization error.
	case pgerrcode.DeadlockDetected:
		return &ErrSerializationError{
			DBError: pqErr,
		}

	// Handle schema error.
	case pgerrcode.UndefinedColumn, pgerrcode.UndefinedTable:
		return &ErrSchemaError{
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

// IsSerializationOrDeadlockError returns true if the given error is either a
// deadlock error or a serialization error.
//
// DeadlockDetected errors are already mapped to ErrSerializationError above,
// so checking for serialization errors is sufficient on no-SQLite targets.
func IsSerializationOrDeadlockError(err error) bool {
	return IsSerializationError(err)
}

// ErrSchemaError is an error type which represents a database agnostic error
// that the schema of the database is incorrect for the given query.
type ErrSchemaError struct {
	DBError error
}

// Unwrap returns the wrapped error.
func (e ErrSchemaError) Unwrap() error {
	return e.DBError
}

// Error returns the error message.
func (e ErrSchemaError) Error() string {
	return e.DBError.Error()
}

// IsSchemaError returns true if the given error is a schema error.
func IsSchemaError(err error) bool {
	var schemaError *ErrSchemaError
	return errors.As(err, &schemaError)
}
