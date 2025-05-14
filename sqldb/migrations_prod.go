//go:build !test_db_postgres && !test_db_sqlite

package sqldb

var migrationAdditions []MigrationConfig
