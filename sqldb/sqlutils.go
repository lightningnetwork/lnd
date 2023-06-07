package sqldb

import (
	"database/sql"

	"golang.org/x/exp/constraints"
)

// sqlInt32 turns a numerical integer type into the NullInt32 that sql/sqlc
// uses when an integer field can be permitted to be NULL.
//
// We use this constraints.Integer constraint here which maps to all signed and
// unsigned integer types.
func sqlInt32[T constraints.Integer](num T) sql.NullInt32 {
	return sql.NullInt32{
		Int32: int32(num),
		Valid: true,
	}
}

// extractSqlInt32 turns a NullInt32 into a numerical type. This can be useful
// when reading directly from the database, as this function handles extracting
// the inner value from the "option"-like struct.
func extractSqlInt32[T constraints.Integer](num sql.NullInt32) T {
	return T(num.Int32)
}

// sqlStr turns a string into the NullString that sql/sqlc uses when a string
// can be permitted to be NULL.
func sqlStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{
		String: s,
		Valid:  true,
	}
}
