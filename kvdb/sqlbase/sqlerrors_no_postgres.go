//go:build !kvdb_postgres

package sqlbase

// parsePostgresError attempts to parse a postgres error as a database agnostic
// SQL error.
func parsePostgresError(err error) error {
	return nil
}
