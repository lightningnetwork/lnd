//go:build test_db_postgres || test_db_sqlite || test_native_sql

package itest

// bolt12DevMigrations is true because the SQL test tags add the development
// migrations that hold the BOLT 12 tables. The build constraint matches the
// one of sqldb/migrations_dev.go.
const bolt12DevMigrations = true
