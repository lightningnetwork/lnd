//go:build test_db_postgres || test_db_sqlite || test_native_sql

package sqldb

var migrationAdditions = []MigrationConfig{
	{
		Name:          "000009_payments",
		Version:       11,
		SchemaVersion: 9,
	},
	{
		Name:          "000010_payment_duplicates",
		Version:       12,
		SchemaVersion: 10,
	},
	{
		Name:          "kv_payments_migration",
		Version:       13,
		SchemaVersion: 10,
		// A migration function may be attached to this
		// migration to migrate KV payments to the native SQL
		// schema. This is optional and can be disabled by the
		// user if necessary.
	},
}
