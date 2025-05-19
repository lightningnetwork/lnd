package sqlmodel

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/lightningnetwork/lnd/sqldb/v2"
	"github.com/lightningnetwork/lnd/sqlmodel/sqlc"
)

type postMigrationCheck func(context.Context, *sqlc.Queries) error

// makePostStepCallbacks turns the post migration checks into a map of post
// step callbacks that can be used with the migrate package. The keys of the map
// are the migration versions, and the values are the callbacks that will be
// executed after the migration with the corresponding version is applied.
func makePostStepCallbacks(db *sqldb.BaseDB,
	c map[uint]postMigrationCheck) map[uint]migrate.PostStepCallback {

	queries := sqlc.NewForType(db, db.BackendType)
	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) *sqlc.Queries {
			return queries.WithTx(tx)
		},
	)

	var (
		ctx               = context.Background()
		postStepCallbacks = make(map[uint]migrate.PostStepCallback)
	)
	for version, check := range c {
		runCheck := func(m *migrate.Migration, q *sqlc.Queries) error {
			log.Infof("Running post-migration check for version %d",
				version)
			start := time.Now()

			err := check(ctx, q)
			if err != nil {
				return fmt.Errorf("post-migration "+
					"check failed for version %d: "+
					"%w", version, err)
			}

			log.Infof("Post-migration check for version %d "+
				"completed in %v", version, time.Since(start))

			return nil
		}

		// We ignore the actual driver that's being returned here, since
		// we use migrate.NewWithInstance() to create the migration
		// instance from our already instantiated database backend that
		// is also passed into this function.
		postStepCallbacks[version] = func(m *migrate.Migration,
			_ database.Driver) error {

			return executor.ExecTx(
				ctx, sqldb.NewWriteTx(),
				func(q *sqlc.Queries) error {
					return runCheck(m, q)
				}, sqldb.NoOpReset,
			)
		}
	}

	return postStepCallbacks
}

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
		Schemas:          sqlSchemas,

		LatestMigrationVersion: 7,

		MakePostMigrationChecks: func(
			db *sqldb.BaseDB) (map[uint]migrate.PostStepCallback,
			error) {

			return makePostStepCallbacks(
				db, map[uint]postMigrationCheck{
					7: func(ctx context.Context,
						querier *sqlc.Queries) error {

						// TODO(guggero): This is where
						// we would migrate the KV
						// invoices to the native SQL
						// schema. Obviously needs to be
						// refactored, as we can't call
						// into much of that code here.
						return nil
					},
				},
			), nil
		},

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
		Schemas:          sqlSchemas,

		LatestMigrationVersion: 1,

		MakePostMigrationChecks: func(
			db *sqldb.BaseDB) (map[uint]migrate.PostStepCallback,
			error) {

			return makePostStepCallbacks(db, nil), nil
		},

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
