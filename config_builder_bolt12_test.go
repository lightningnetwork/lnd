//go:build dev && test_db_sqlite

package lnd

import (
	"crypto/rand"
	"database/sql"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/bolt12handler/envelope"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestSQLInvoiceStoreWithBolt12 verifies that the SQL invoice store of a node
// with BOLT 12 offers enabled stores and reads a BOLT 12 invoice.
func TestSQLInvoiceStoreWithBolt12(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := sqldb.NewTestSqliteDB(t).BaseDB
	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) invoices.SQLInvoiceQueries {
			return db.WithTx(tx)
		},
	)

	protocol := &lncfg.ProtocolOptions{}
	protocol.Bolt12Offers = true
	store := invoices.NewSQLStore(
		executor, clock.NewTestClock(time.Unix(1, 0)),
		sqlInvoiceStoreOptions(protocol)...,
	)

	var (
		preimage lntypes.Preimage
		payAddr  [32]byte
	)
	_, err := rand.Read(preimage[:])
	require.NoError(t, err)
	_, err = rand.Read(payAddr[:])
	require.NoError(t, err)

	envelopeBytes, err := envelope.Encode(&envelope.Data{
		CreatedAt: 1,
		Amount:    5000,
	})
	require.NoError(t, err)

	invoice := &invoices.Invoice{
		CreationDate: time.Unix(1, 0),
		Terms: invoices.ContractTerm{
			Expiry:          time.Hour,
			PaymentPreimage: &preimage,
			PaymentAddr:     payAddr,
			Value:           lnwire.MilliSatoshi(5000),
			Features: lnwire.NewFeatureVector(
				nil, lnwire.Features,
			),
		},
		IsBolt12:       true,
		Bolt12Envelope: envelopeBytes,
	}
	payHash := preimage.Hash()

	_, err = store.AddInvoice(ctx, invoice, payHash)
	require.NoError(t, err)

	got, err := store.LookupInvoice(ctx, invoices.InvoiceRefByHash(payHash))
	require.NoError(t, err)
	require.True(t, got.IsBolt12)
}
