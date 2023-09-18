//go:build !kvdb_sqlite || (windows && (arm || 386)) || (linux && (ppc64 || mips || mipsle || mips64))

package sqlbase

// parseSqliteError attempts to parse a sqlite error as a database agnostic
// SQL error.
func parseSqliteError(err error) error {
	return nil
}
