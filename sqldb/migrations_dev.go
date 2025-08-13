//go:build test_db_postgres || test_db_sqlite || test_native_sql

package sqldb

var migrationAdditions = []MigrationConfig{
	{
		Name:          "000008_graph",
		Version:       9,
		SchemaVersion: 8,
	},
	{
		Name:          "kv_graph_migration",
		Version:       10,
		SchemaVersion: 8,
		// A migration function may be attached to this
		// migration to migrate KV graph to the native SQL
		// schema. This is optional and can be disabled by the
		// user if necessary.
	},
}
