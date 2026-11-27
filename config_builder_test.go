package lnd

import (
	"crypto/rand"
	"database/sql"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestSQLInvoiceStoreWithoutBolt12 verifies that the SQL invoice store of a
// node without BOLT 12 offers reads a BOLT 11 invoice. A release build has no
// BOLT 12 side table, so the store must not query it.
func TestSQLInvoiceStoreWithoutBolt12(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := sqldb.NewTestSqliteDB(t).BaseDB
	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) invoices.SQLInvoiceQueries {
			return db.WithTx(tx)
		},
	)

	store := invoices.NewSQLStore(
		executor, clock.NewTestClock(time.Unix(1, 0)),
		sqlInvoiceStoreOptions(&lncfg.ProtocolOptions{})...,
	)

	var (
		preimage lntypes.Preimage
		payAddr  [32]byte
	)
	_, err := rand.Read(preimage[:])
	require.NoError(t, err)
	_, err = rand.Read(payAddr[:])
	require.NoError(t, err)

	invoice := &invoices.Invoice{
		CreationDate:   time.Unix(1, 0),
		PaymentRequest: []byte("lnbc1test"),
		Terms: invoices.ContractTerm{
			Expiry:          time.Hour,
			PaymentPreimage: &preimage,
			PaymentAddr:     payAddr,
			Value:           lnwire.MilliSatoshi(5000),
			Features: lnwire.NewFeatureVector(
				nil, lnwire.Features,
			),
		},
	}
	payHash := preimage.Hash()

	_, err = store.AddInvoice(ctx, invoice, payHash)
	require.NoError(t, err)

	got, err := store.LookupInvoice(ctx, invoices.InvoiceRefByHash(payHash))
	require.NoError(t, err)
	require.False(t, got.IsBolt12)

	_, err = store.QueryInvoices(ctx, invoices.InvoiceQuery{
		NumMaxInvoices: 10,
	})
	require.NoError(t, err)
}
