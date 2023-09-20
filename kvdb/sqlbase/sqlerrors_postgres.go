//go:build kvdb_postgres

package sqlbase

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
)

// parsePostgresError attempts to parse a postgres error as a database agnostic
// SQL error.
func parsePostgresError(err error) error {
	if err == nil {
		return nil
	}

	// Sometimes the error won't be properly wrapped, so we'll need to
	// inspect raw error itself to detect something we can wrap properly.
	const postgresErrMsg = "could not serialize access"
	if strings.Contains(err.Error(), postgresErrMsg) {
		return &ErrSerializationError{
			DBError: err,
		}
	}

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
