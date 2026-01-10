//go:build test_db_postgres || test_db_sqlite

package migration1

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lightningnetwork/lnd/payments/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// setupTestSQLDB creates a SQLStore-backed test database.
func setupTestSQLDB(t testing.TB, opts ...OptionModifier) *SQLStore {
	t.Helper()

	db, _ := NewTestDB(t, opts...)
	sqlStore, ok := db.(*SQLStore)
	require.True(t, ok)

	return sqlStore
}

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
