//go:build kvdb_postgres

package sqlbase

import (
	"errors"
	"fmt"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
)

// parsePostgresError attempts to parse a postgres error as a database agnostic
// SQL error.
func parsePostgresError(err error) error {
	var pqErr *pgconn.PgError
	if !errors.As(err, &pqErr) {
		return nil
	}

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

	default:
		return fmt.Errorf("unknown postgres error: %w", pqErr)
	}
}
