//go:build !test_db_postgres && !test_db_sqlite && !test_native_sql

package itest

// bolt12DevMigrations is false because without a SQL test tag the build has
// no development migrations, so it has no BOLT 12 tables. The build
// constraint matches the one of sqldb/migrations_prod.go.
const bolt12DevMigrations = false
