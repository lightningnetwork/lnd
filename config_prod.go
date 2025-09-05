//go:build !test_native_sql

package lnd

import (
	"context"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

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
