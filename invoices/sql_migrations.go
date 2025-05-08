package invoices

import (
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/lightningnetwork/lnd/sqldb"
)

var (
	InvoicesMigrations = sqldb.MigrationStream{
		// MigrateTableName is the name of the table used to track
		// migrations. This is used by the golang-migrate library to
		// determine which migrations have been applied to the database.
		// We need to use the default name here, because this might
		// already have been applied to some users. Any new migration
		// stream will use a different name to distinguish between
		// different migration streams.
		MigrateTableName: pgx.DefaultMigrationsTable,
		SQLFileDirectory: "sqlc/migrations/invoices",

		// Configs defines a list of migrations to be applied to the
		// database. Each migration is assigned a version number, determining
		// its execution order.
		// The schema version, tracked by golang-migrate, ensures migrations are
		// applied to the correct schema. For migrations involving only schema
		// changes, the migration function can be left nil. For custom
		// migrations an implemented migration function is required.
		//
		// NOTE: The migration function may have runtime dependencies, which
		// must be injected during runtime.
		Configs: []sqldb.MigrationConfig{
			{
				Name:          "000001_invoices",
				Version:       1,
				SchemaVersion: 1,
			},
			{
				Name:          "000002_amp_invoices",
				Version:       2,
				SchemaVersion: 2,
			},
			{
				Name:          "000003_invoice_events",
				Version:       3,
				SchemaVersion: 3,
			},
			{
				Name:          "000004_invoice_expiry_fix",
				Version:       4,
				SchemaVersion: 4,
			},
			{
				Name:          "000005_migration_tracker",
				Version:       5,
				SchemaVersion: 5,
			},
			{
				Name:          "000006_invoice_migration",
				Version:       6,
				SchemaVersion: 6,
			},
			{
				Name:          "kv_invoice_migration",
				Version:       7,
				SchemaVersion: 6,
				// A migration function is may be attached to this
				// migration to migrate KV invoices to the native SQL
				// schema. This is optional and can be disabled by the
				// user if necessary.
			},
		},
	}
)
