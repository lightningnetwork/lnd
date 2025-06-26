//go:build !test_native_sql

package lnd

import (
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb"
)

// getGraphStore returns a graphdb.V1Store backed by a graphdb.KVStore
// implementation.
func (d *DefaultDatabaseBuilder) getGraphStore(_ *sqldb.BaseDB,
	kvBackend kvdb.Backend,
	opts ...graphdb.StoreOptionModifier) (graphdb.V1Store, error) {

	return graphdb.NewKVStore(kvBackend, opts...)
}
