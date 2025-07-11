//go:build !test_native_sql

package lnd

import (
	"context"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// getGraphStore returns a graphdb.V1Store backed by a graphdb.KVStore
// implementation.
func (d *DefaultDatabaseBuilder) getGraphStore(_ *sqldb.BaseDB,
	kvBackend kvdb.Backend,
	opts ...graphdb.StoreOptionModifier) (graphdb.V1Store, error) {

	return graphdb.NewKVStore(kvBackend, opts...)
}

// getSQLMigration returns a migration function for the given version.
//
// NOTE: this is a no-op for the production build since all migrations that are
// in production will also be in development builds, and so they are not
// defined behind a build tag.
func getSQLMigration(ctx context.Context, version int,
	kvBackend kvdb.Backend,
	chain chainhash.Hash) (func(tx *sqlc.Queries) error, bool) {

	return nil, false
}
