//go:build !test_native_sql

package lnd

import (
	"context"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

func getSQLMigration(ctx context.Context, version int,
	kvGraphStore *graphdb.KVStore) (func(tx *sqlc.Queries) error, bool) {

	return nil, false
}

func (d *DefaultDatabaseBuilder) getGraphStore(_ *sqldb.BaseDB,
	kvGraphStore *graphdb.KVStore,
	_ ...graphdb.StoreOptionModifier) (graphdb.V1Store, error) {

	return kvGraphStore, nil
}
