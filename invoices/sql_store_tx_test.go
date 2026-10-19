package invoices

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestInvoicesSettledSinceSingleConnection ensures that all queries in the
// settled-invoice scan use its transaction-bound connection.
func TestInvoicesSettledSinceSingleConnection(t *testing.T) {
	db := sqldb.NewTestSqliteDB(t).BaseDB
	db.DB.SetMaxOpenConns(1)
	db.DB.SetMaxIdleConns(1)

	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLInvoiceQueries {
			return db.WithTx(tx)
		},
	)
	store := NewSQLStore(executor, clock.NewTestClock(time.Now()))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	invoices, err := store.InvoicesSettledSince(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, invoices)
}
