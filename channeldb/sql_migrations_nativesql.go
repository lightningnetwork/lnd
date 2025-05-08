//go:build nativesql

package channeldb

import "github.com/lightningnetwork/lnd/sqldb"

var (
	PaymentsMigrations = sqldb.MigrationStream{
		// MigrateTableName is the name of the table used to track
		// migrations. This is used by the golang-migrate library to
		// determine which migrations have been applied to the database.
		// We need to use the default name here, because this might
		// already have been applied to some users. Any new migration
		// stream will use a different name to distinguish between
		// different migration streams.
		MigrateTableName: "schema_migrations_payments",
		SQLFileDirectory: "sqlc/migrations/payments",

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
				Name:          "000001_payments",
				Version:       1,
				SchemaVersion: 1,
			},
		},
	}
)
