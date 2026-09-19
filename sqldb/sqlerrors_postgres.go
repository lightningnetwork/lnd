package sqldb

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// withPostgresDetail returns an error that also carries the Detail field of the
// given Postgres error, if it has one. The error string that the driver builds
// leaves that field out, and for a transaction conflict it is the only thing
// that says which kind of conflict was hit: an abort that only serializable
// isolation raises spells out the reason code of the pivot that was cancelled,
// whereas a plain write-write conflict just reports a concurrent update. Being
// able to tell the two apart is what says how much of the abort pressure a
// deployment sees would go away by relaxing the isolation level.
//
// NOTE: This must only ever be applied to the errors that report a transaction
// conflict. For any other error, and for a constraint violation in particular,
// the detail spells out the offending column values, which for our schemas
// means things like payment session keys, invoice hashes and raw key/value
// bucket keys. None of that belongs in a log line, let alone in an error that
// may travel back over an RPC.
//
// This lives in its own file, free of build tags, so that the two build
// specific copies of the error parsing below it share a single definition of
// it.
func withPostgresDetail(pqErr *pgconn.PgError) error {
	if pqErr.Detail == "" {
		return pqErr
	}

	return fmt.Errorf("%w (detail: %s)", pqErr, pqErr.Detail)
}
