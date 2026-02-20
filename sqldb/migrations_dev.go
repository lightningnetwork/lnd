//go:build test_db_postgres || test_db_sqlite || test_native_sql

package sqldb

var migrationAdditions = []MigrationConfig{
	{
		Name:          "000010_payments",
		Version:       12,
		SchemaVersion: 10,
	},
	{
		Name:          "000011_payment_duplicates",
		Version:       13,
		SchemaVersion: 11,
	},
	{
		Name:          "kv_payments_migration",
		Version:       14,
		SchemaVersion: 11,
		// A migration function may be attached to this
		// migration to migrate KV payments to the native SQL
		// schema. This is optional and can be disabled by the
		// user if necessary.
	},
	{
		Name:          "000012_drop_redundant_invoice_indexes",
		Version:       15,
		SchemaVersion: 12,
	},
}
