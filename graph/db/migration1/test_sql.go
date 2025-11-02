//go:build test_db_postgres || test_db_sqlite

package migration1

import (
	"context"
	"database/sql"

	"github.com/lightningnetwork/lnd/graph/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/sqldb"
)

// testBatchedSQLQueries is a simple implementation of BatchedSQLQueries for
// testing.
type testBatchedSQLQueries struct {
	db *sql.DB
	*sqlc.Queries
}

// ExecTx implements the transaction execution logic.
func (t *testBatchedSQLQueries) ExecTx(ctx context.Context,
	txOpts sqldb.TxOptions, txBody func(SQLQueries) error,
	reset func()) error {

	sqlOptions := sql.TxOptions{
		Isolation: sql.LevelSerializable,
		ReadOnly:  txOpts.ReadOnly(),
	}

	tx, err := t.db.BeginTx(ctx, &sqlOptions)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()

	reset()
	queries := sqlc.New(tx)

	return txBody(queries)
}
