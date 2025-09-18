//go:build test_db_postgres || test_db_sqlite || test_native_sql

package sqldb

var migrationAdditions = []MigrationConfig{
	{
		Name:          "kv_graph_migration",
		Version:       11,
		SchemaVersion: 9,
		// A migration function may be attached to this
		// migration to migrate KV graph to the native SQL
		// schema. This is optional and can be disabled by the
		// user if necessary.
	},
	{
		Name:          "000009_payments",
		Version:       12,
		SchemaVersion: 9,
	},
}
