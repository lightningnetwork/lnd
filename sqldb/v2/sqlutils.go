package sqldb

import (
	"database/sql"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"golang.org/x/exp/constraints"
)

var (
	// MaxValidSQLTime is the maximum valid time that can be rendered as a
	// time string and can be used for comparisons in SQL.
	MaxValidSQLTime = time.Date(9999, 12, 31, 23, 59, 59, 999999, time.UTC)
)

// NoOpReset is a no-op function that can be used as a default
// reset function ExecTx calls.
var NoOpReset = func() {}

// SQLInt16 turns a numerical integer type into the NullInt16 that sql/sqlc
// uses when an integer field can be permitted to be NULL.
//
// We use this constraints.Integer constraint here which maps to all signed and
// unsigned integer types.
func SQLInt16[T constraints.Integer](num T) sql.NullInt16 {
	return sql.NullInt16{
		Int16: int16(num),
		Valid: true,
	}
}

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

// SqlOptInt32 turns an option of a numerical integer type into the NullInt32
// that sql/sqlc uses when an integer field can be permitted to be NULL.
func SqlOptInt32[T constraints.Integer](num fn.Option[T]) sql.NullInt32 {
	return fn.MapOptionZ(num, func(num T) sql.NullInt32 {
		return sql.NullInt32{
			Int32: int32(num),
			Valid: true,
		}
	})
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

// SqlBool turns a boolean into the NullBool that sql/sqlc uses when a boolean
// field can be permitted to be NULL.
func SqlBool(b bool) sql.NullBool {
	return sql.NullBool{
		Bool:  b,
		Valid: true,
	}
}

// SQLStr turns a string into the NullString that sql/sqlc uses when a string
// can be permitted to be NULL.
//
// NOTE: If the input string is empty, it returns a NullString with Valid set to
// false. If this is not the desired behavior, consider using SQLStrValid
// instead.
func SQLStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{
		String: s,
		Valid:  true,
	}
}

// SQLStrValid turns a string into the NullString that sql/sqlc uses when a
// string can be permitted to be NULL.
//
// NOTE: Valid is always set to true, even if the input string is empty.
func SQLStrValid(s string) sql.NullString {
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

// ExtractSqlInt64 turns a NullInt64 into a numerical type. This can be useful
// when reading directly from the database, as this function handles extracting
// the inner value from the "option"-like struct.
func ExtractSqlInt64[T constraints.Integer](num sql.NullInt64) T {
	return T(num.Int64)
}

// ExtractSqlInt32 turns a NullInt32 into a numerical type. This can be useful
// when reading directly from the database, as this function handles extracting
// the inner value from the "option"-like struct.
func ExtractSqlInt32[T constraints.Integer](num sql.NullInt32) T {
	return T(num.Int32)
}

// ExtractOptSqlInt32 turns a NullInt32 into an option of a numerical type.
func ExtractOptSqlInt32[T constraints.Integer](num sql.NullInt32) fn.Option[T] {
	if !num.Valid {
		return fn.None[T]()
	}

	result := T(num.Int32)
	return fn.Some(result)
}

// ExtractSqlInt16 turns a NullInt16 into a numerical type. This can be useful
// when reading directly from the database, as this function handles extracting
// the inner value from the "option"-like struct.
func ExtractSqlInt16[T constraints.Integer](num sql.NullInt16) T {
	return T(num.Int16)
}

// ExtractBool turns a NullBool into a boolean. This can be useful when reading
// directly from the database, as this function handles extracting the inner
// value from the "option"-like struct.
func ExtractBool(b sql.NullBool) bool {
	return b.Bool
}
