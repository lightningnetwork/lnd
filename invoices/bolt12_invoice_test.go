//go:build test_db_sqlite

package invoices_test

import (
	"crypto/rand"
	"database/sql"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/bolt12handler/envelope"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// bolt12TestDB bundles an invoice store with the raw queries the tests use to
// set up offers and to look into the BOLT 12 side table.
type bolt12TestDB struct {
	store   invpkg.InvoiceDB
	queries *sqlc.Queries
}

// makeBolt12TestDB creates a SQLite-backed invoice store. The SQLite test
// build applies the development migrations, so the BOLT 12 side table exists
// whatever the options are.
func makeBolt12TestDB(t *testing.T,
	opts ...invpkg.SQLStoreOption) *bolt12TestDB {

	t.Helper()

	db := sqldb.NewTestSqliteDB(t).BaseDB

	executor := sqldb.NewTransactionExecutor(
		db,
		func(tx *sql.Tx) invpkg.SQLInvoiceQueries {
			return db.WithTx(tx)
		},
	)

	testClock := clock.NewTestClock(time.Unix(1, 0))

	return &bolt12TestDB{
		store:   invpkg.NewSQLStore(executor, testClock, opts...),
		queries: db.Queries,
	}
}

// insertTestOffer stores an offer and returns its database ID and hash.
func insertTestOffer(t *testing.T, db *bolt12TestDB) (int64, [32]byte) {
	t.Helper()

	var hash [32]byte
	_, err := rand.Read(hash[:])
	require.NoError(t, err)

	id, err := db.queries.InsertOffer(t.Context(), sqlc.InsertOfferParams{
		Hash:      hash[:],
		Encoded:   "lno1test",
		CreatedAt: time.Unix(1, 0),
	})
	require.NoError(t, err)

	return id, hash
}

// testEnvelope returns the envelope data of a BOLT 12 invoice for the offer
// with the given hash, or for no offer when the hash is None.
func testEnvelope(offerHash fn.Option[[32]byte]) *envelope.Data {
	var payerID [33]byte
	payerID[0] = 0x03
	for i := 1; i < 33; i++ {
		payerID[i] = byte(i + 50)
	}

	return &envelope.Data{
		OfferHash: offerHash,
		PayerID:   payerID,
		CreatedAt: 1,
		Amount:    10000,
		Quantity:  3,
		PayerNote: []byte("tip"),
	}
}

// newBolt12Invoice returns a BOLT 12 invoice for the offer with the given ID
// that carries the given envelope, in the form that HTLC settlement rebuilds.
// It has no payment request.
func newBolt12Invoice(t *testing.T, offerID fn.Option[int64],
	data *envelope.Data) (*invpkg.Invoice, lntypes.Hash) {

	t.Helper()

	var (
		preimage lntypes.Preimage
		payAddr  [32]byte
	)
	_, err := rand.Read(preimage[:])
	require.NoError(t, err)
	_, err = rand.Read(payAddr[:])
	require.NoError(t, err)

	encoded, err := envelope.Encode(data)
	require.NoError(t, err)

	invoice := &invpkg.Invoice{
		CreationDate: time.Unix(1, 0),
		Terms: invpkg.ContractTerm{
			Expiry:          7200 * time.Second,
			PaymentPreimage: &preimage,
			PaymentAddr:     payAddr,
			Value:           lnwire.MilliSatoshi(10000),
			Features:        emptyFeatures,
		},
		IsBolt12:       true,
		OfferID:        offerID,
		Bolt12Envelope: encoded,
	}

	return invoice, preimage.Hash()
}

// requireEnvelopeFields asserts that the invoice carries the typed fields of
// the envelope data.
func requireEnvelopeFields(t *testing.T, invoice invpkg.Invoice,
	data *envelope.Data) {

	t.Helper()

	require.True(t, invoice.IsBolt12)
	require.NotEmpty(t, invoice.Bolt12Envelope)
	require.Equal(t, data.PayerID[:], invoice.InvreqPayerID)
	require.Equal(t, data.Quantity, invoice.InvreqQuantity)
	require.Equal(t, data.PayerNote, invoice.InvreqPayerNote)

	var offerHash []byte
	data.OfferHash.WhenSome(func(hash [32]byte) {
		offerHash = hash[:]
	})
	require.Equal(t, offerHash, invoice.OfferHash)
}

// TestBolt12InvoiceRoundTrip verifies that the store writes the envelope and
// fills the typed BOLT 12 fields from it on read. An envelope without an offer
// hash gives an empty offer hash. A request without a quantity or a payer note
// gives a zero quantity and no note.
func TestBolt12InvoiceRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		withOffer bool
		modify    func(data *envelope.Data)
	}{
		{
			name:      "with offer",
			withOffer: true,
			modify:    func(*envelope.Data) {},
		},
		{
			name:   "without offer",
			modify: func(*envelope.Data) {},
		},
		{
			name:      "without quantity and payer note",
			withOffer: true,
			modify: func(data *envelope.Data) {
				data.Quantity = 0
				data.PayerNote = nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := makeBolt12TestDB(t, invpkg.WithBolt12())
			ctx := t.Context()

			offerID := fn.None[int64]()
			offerHash := fn.None[[32]byte]()
			if tc.withOffer {
				id, hash := insertTestOffer(t, db)
				offerID, offerHash = fn.Some(id), fn.Some(hash)
			}
			data := testEnvelope(offerHash)
			tc.modify(data)
			invoice, payHash := newBolt12Invoice(t, offerID, data)

			_, err := db.store.AddInvoice(ctx, invoice, payHash)
			require.NoError(t, err)

			ref := invpkg.InvoiceRefByHash(payHash)
			got, err := db.store.LookupInvoice(ctx, ref)
			require.NoError(t, err)

			require.Equal(t, offerID, got.OfferID)
			require.Equal(
				t, invoice.Bolt12Envelope, got.Bolt12Envelope,
			)
			requireEnvelopeFields(t, got, data)
		})
	}
}

// TestBolt11InvoiceWithBolt12Store verifies that a BOLT 11 invoice has no side
// table row and reads back without BOLT 12 data.
func TestBolt11InvoiceWithBolt12Store(t *testing.T) {
	t.Parallel()

	db := makeBolt12TestDB(t, invpkg.WithBolt12())
	ctx := t.Context()

	invoice, err := randInvoice(lnwire.MilliSatoshi(5000))
	require.NoError(t, err)

	payHash := invoice.Terms.PaymentPreimage.Hash()

	addIndex, err := db.store.AddInvoice(ctx, invoice, payHash)
	require.NoError(t, err)

	ref := invpkg.InvoiceRefByHash(payHash)
	got, err := db.store.LookupInvoice(ctx, ref)
	require.NoError(t, err)

	require.False(t, got.IsBolt12)
	require.Zero(t, got.InvreqQuantity)
	require.True(t, got.OfferID.IsNone())
	require.Nil(t, got.OfferHash)
	require.Nil(t, got.InvreqPayerID)
	require.Nil(t, got.InvreqPayerNote)
	require.Nil(t, got.Bolt12Envelope)

	_, err = db.queries.FetchBolt12Invoice(ctx, int64(addIndex))
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestBolt12InvoiceStoreDisabled verifies that a store without BOLT 12 refuses
// a BOLT 12 invoice and stores nothing.
func TestBolt12InvoiceStoreDisabled(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	db := makeBolt12TestDB(t)
	offerID, offerHash := insertTestOffer(t, db)
	invoice, payHash := newBolt12Invoice(
		t, fn.Some(offerID), testEnvelope(fn.Some(offerHash)),
	)

	_, err := db.store.AddInvoice(ctx, invoice, payHash)
	require.ErrorIs(t, err, invpkg.ErrBolt12Disabled)

	_, err = db.store.LookupInvoice(ctx, invpkg.InvoiceRefByHash(payHash))
	require.ErrorIs(t, err, invpkg.ErrInvoiceNotFound)
}

// TestBolt12InvoiceDeleteCascades verifies that deleting a BOLT 12 invoice also
// deletes its side table row. The registry does this for an open BOLT 12
// invoice whose HTLCs all failed.
func TestBolt12InvoiceDeleteCascades(t *testing.T) {
	t.Parallel()

	db := makeBolt12TestDB(t, invpkg.WithBolt12())
	ctx := t.Context()

	offerID, offerHash := insertTestOffer(t, db)
	invoice, payHash := newBolt12Invoice(
		t, fn.Some(offerID), testEnvelope(fn.Some(offerHash)),
	)

	addIndex, err := db.store.AddInvoice(ctx, invoice, payHash)
	require.NoError(t, err)

	_, err = db.queries.FetchBolt12Invoice(ctx, int64(addIndex))
	require.NoError(t, err)

	payAddr := invoice.Terms.PaymentAddr
	err = db.store.DeleteInvoice(ctx, []invpkg.InvoiceDeleteRef{{
		PayHash:  payHash,
		PayAddr:  &payAddr,
		AddIndex: addIndex,
	}})
	require.NoError(t, err)

	_, err = db.queries.FetchBolt12Invoice(ctx, int64(addIndex))
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestBolt12InvoiceRejectsAMP verifies that the store refuses an invoice that
// is both a BOLT 12 and an AMP invoice, because the read path does not load
// the BOLT 12 data of an AMP invoice.
func TestBolt12InvoiceRejectsAMP(t *testing.T) {
	t.Parallel()

	db := makeBolt12TestDB(t, invpkg.WithBolt12())
	ctx := t.Context()

	invoice, payHash := newBolt12Invoice(
		t, fn.None[int64](), testEnvelope(fn.None[[32]byte]()),
	)
	invoice.Terms.Features = ampFeatures
	invoice.Terms.PaymentPreimage = nil

	_, err := db.store.AddInvoice(ctx, invoice, payHash)
	require.ErrorIs(t, err, invpkg.ErrBolt12AMP)
}

// TestBolt12InvoiceEnvelopeChecks verifies that the store refuses a BOLT 12
// invoice without an envelope or with one that does not decode, so that every
// later read of the invoice can decode its envelope.
func TestBolt12InvoiceEnvelopeChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		envelope []byte
		wantErr  error
	}{
		{
			name:    "no envelope",
			wantErr: invpkg.ErrBolt12NoEnvelope,
		},
		{
			name:     "envelope that does not decode",
			envelope: []byte{0x02, 0x21, 0x01},
			wantErr:  invpkg.ErrBolt12BadEnvelope,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := makeBolt12TestDB(t, invpkg.WithBolt12())
			invoice, payHash := newBolt12Invoice(
				t, fn.None[int64](),
				testEnvelope(fn.None[[32]byte]()),
			)
			invoice.Bolt12Envelope = tc.envelope

			_, err := db.store.AddInvoice(
				t.Context(), invoice, payHash,
			)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestBolt12InvoiceListPaths verifies that every list call returns the BOLT 12
// data of a BOLT 12 invoice.
func TestBolt12InvoiceListPaths(t *testing.T) {
	t.Parallel()

	db := makeBolt12TestDB(t, invpkg.WithBolt12())
	ctx := t.Context()

	// The calls with an index return the invoices after it, so a BOLT 11
	// invoice takes the first add and settle index.
	bolt11, err := randInvoice(lnwire.MilliSatoshi(5000))
	require.NoError(t, err)
	bolt11Hash := bolt11.Terms.PaymentPreimage.Hash()
	_, err = db.store.AddInvoice(ctx, bolt11, bolt11Hash)
	require.NoError(t, err)
	_, err = db.store.UpdateInvoice(
		ctx, invpkg.InvoiceRefByHash(bolt11Hash), nil,
		getUpdateInvoice(0, bolt11.Terms.Value),
	)
	require.NoError(t, err)

	offerID, offerHash := insertTestOffer(t, db)
	data := testEnvelope(fn.Some(offerHash))
	invoice, payHash := newBolt12Invoice(t, fn.Some(offerID), data)

	_, err = db.store.AddInvoice(ctx, invoice, payHash)
	require.NoError(t, err)

	requireFields := func(invoice invpkg.Invoice) {
		t.Helper()

		require.Equal(t, fn.Some(offerID), invoice.OfferID)
		requireEnvelopeFields(t, invoice, data)
	}

	pending, err := db.store.FetchPendingInvoices(ctx)
	require.NoError(t, err)
	require.Contains(t, pending, payHash)
	requireFields(pending[payHash])

	added, err := db.store.InvoicesAddedSince(ctx, 1)
	require.NoError(t, err)
	require.Len(t, added, 1)
	requireFields(added[0])

	resp, err := db.store.QueryInvoices(ctx, invpkg.InvoiceQuery{
		NumMaxInvoices: 10,
	})
	require.NoError(t, err)
	require.Len(t, resp.Invoices, 2)
	requireFields(resp.Invoices[1])

	settled, err := db.store.UpdateInvoice(
		ctx, invpkg.InvoiceRefByHash(payHash), nil,
		getUpdateInvoice(1, invoice.Terms.Value),
	)
	require.NoError(t, err)
	require.Equal(t, invpkg.ContractSettled, settled.State)
	requireFields(*settled)

	settledSince, err := db.store.InvoicesSettledSince(ctx, 1)
	require.NoError(t, err)
	require.Len(t, settledSince, 1)
	requireFields(settledSince[0])
}

// TestBolt12InvoiceDeleteCanceledCascades verifies that deleting the canceled
// invoices also deletes their side table rows.
func TestBolt12InvoiceDeleteCanceledCascades(t *testing.T) {
	t.Parallel()

	db := makeBolt12TestDB(t, invpkg.WithBolt12())
	ctx := t.Context()

	offerID, offerHash := insertTestOffer(t, db)
	invoice, payHash := newBolt12Invoice(
		t, fn.Some(offerID), testEnvelope(fn.Some(offerHash)),
	)

	addIndex, err := db.store.AddInvoice(ctx, invoice, payHash)
	require.NoError(t, err)

	cancel := func(*invpkg.Invoice) (*invpkg.InvoiceUpdateDesc, error) {
		return &invpkg.InvoiceUpdateDesc{
			UpdateType: invpkg.CancelInvoiceUpdate,
			State: &invpkg.InvoiceStateUpdateDesc{
				NewState: invpkg.ContractCanceled,
			},
		}, nil
	}
	_, err = db.store.UpdateInvoice(
		ctx, invpkg.InvoiceRefByHash(payHash), nil, cancel,
	)
	require.NoError(t, err)

	_, err = db.queries.FetchBolt12Invoice(ctx, int64(addIndex))
	require.NoError(t, err)

	require.NoError(t, db.store.DeleteCanceledInvoices(ctx))

	_, err = db.queries.FetchBolt12Invoice(ctx, int64(addIndex))
	require.ErrorIs(t, err, sql.ErrNoRows)
}
