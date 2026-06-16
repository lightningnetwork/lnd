//go:build !test_native_sql

package lnd

import (
	"context"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// NOTE: This file (together with config_test_native_sql.go) contains
// build-tag-specific overrides that control which backend is used for certain
// stores. If any function in either file switches a store between KV and native
// SQL depending on the build tag, and the corresponding migration is later
// promoted from sqldb/migrations_dev.go into the mainline sqldb/migrations.go,
// you must also update the UseNativeSQL branch in BuildDatabase
// (config_builder.go) to use the native SQL backend for that store. Promoting
// the migration without updating the store means the production build will
// continue writing to the KV backend instead of SQL.

// RunTestSQLMigration is a build tag that indicates whether the test_native_sql
// build tag is set.
var RunTestSQLMigration = false

// getSQLMigration returns a migration function for the given version.
//
// NOTE: this is a no-op for the production build since all migrations that are
// in production will also be in development builds, and so they are not
// defined behind a build tag.
func (d *DefaultDatabaseBuilder) getSQLMigration(ctx context.Context,
	version int, kvBackend kvdb.Backend) (func(tx *sqlc.Queries) error,
	bool) {

	return nil, false
}
