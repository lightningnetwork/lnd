package sqldb

import (
	"database/sql"
	"time"

	"golang.org/x/exp/constraints"
)

// SQLInt32 turns a numerical integer type into the NullInt32 that sql/sqlc
// uses when an integer field can be permitted to be NULL.
//
// We use this constraints.Integer constraint here which maps to all signed and
// unsigned integer types.
func SQLInt32[T constraints.Integer](num T) sql.NullInt32 {
	return sql.NullInt32{
		Int32: int32(num),
		Valid: true,
	}
}

// SQLInt64 turns a numerical integer type into the NullInt64 that sql/sqlc
// uses when an integer field can be permitted to be NULL.
//
// We use this constraints.Integer constraint here which maps to all signed and
// unsigned integer types.
func SQLInt64[T constraints.Integer](num T) sql.NullInt64 {
	return sql.NullInt64{
		Int64: int64(num),
		Valid: true,
	}
}

// SQLStr turns a string into the NullString that sql/sqlc uses when a string
// can be permitted to be NULL.
func SQLStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{
		String: s,
		Valid:  true,
	}
}

// SQLTime turns a time.Time into the NullTime that sql/sqlc uses when a time
// can be permitted to be NULL.
func SQLTime(t time.Time) sql.NullTime {
	return sql.NullTime{
		Time:  t,
		Valid: true,
	}
}
