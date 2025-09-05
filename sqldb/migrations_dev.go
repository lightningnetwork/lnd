//go:build test_db_postgres || test_db_sqlite || test_native_sql

package sqldb

var migrationAdditions []MigrationConfig
