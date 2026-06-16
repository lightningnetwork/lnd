//go:build test_native_sql

package lnd

import (
	"context"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// RunTestSQLMigration is a build tag that indicates whether the test_native_sql
// build tag is set.
var RunTestSQLMigration = true

// getSQLMigration returns a migration function for the given version.
func (d *DefaultDatabaseBuilder) getSQLMigration(_ context.Context,
	version int, _ kvdb.Backend) (func(tx *sqlc.Queries) error,
	bool) {

	switch version {
	default:
		// No version was matched, so we return false to indicate that
		// no migration is known for the given version.
		return nil, false
	}
}
