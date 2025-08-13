//go:build !test_db_postgres && !test_db_sqlite && !test_native_sql

package sqldb

// migrationAdditions is a list of migrations that are added to the
// migrationConfig slice.
//
// NOTE: This should always be empty and instead migrations for production
// should be added into the main line (see migrations.go).
var migrationAdditions []MigrationConfig
