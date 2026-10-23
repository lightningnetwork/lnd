//go:build test_db_postgres || test_db_sqlite || test_native_sql

package sqldb

// migrationAdditions is a list of migrations that are added to the
// migrationConfig slice.
//
// NOTE: This holds migrations whose schema is still in development. Only the
// SQL test builds apply them. A migration moves to the main line (see
// migrations.go) with the next free versions when its schema is final.
var migrationAdditions = []MigrationConfig{
	{
		Name:          "000017_offers",
		Version:       20,
		SchemaVersion: 17,
	},
}
