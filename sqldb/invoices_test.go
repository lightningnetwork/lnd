package sqldb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

var (
	testTimeout = 10 * time.Second

	defautlInvoiceFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.PaymentAddrRequired,
			lnwire.MPPOptional,
		),
		lnwire.Features,
	)

	ampFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.PaymentAddrOptional,
			lnwire.AMPRequired,
		),
		lnwire.Features,
	)

	testNow = time.Unix(1, 0)
)

func newInvoieStoreWithDB(t *testing.T, db *BaseDB) (*InvoiceStore,
	sqlc.Querier) {

	dbTxer := NewTransactionExecutor(db,
		func(tx *sql.Tx) InvoiceQueries {
			return db.WithTx(tx)
		},
	)

	return NewInvoiceStore(dbTxer), db
}

func randInvoice(value lnwire.MilliSatoshi) (*invpkg.Invoice, error) {
	var (
		pre     lntypes.Preimage
		payAddr [32]byte
	)
	if _, err := rand.Read(pre[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(payAddr[:]); err != nil {
		return nil, err
	}

	i := &invpkg.Invoice{
		CreationDate: testNow,
		Terms: invpkg.ContractTerm{
			Expiry:          4000,
			PaymentPreimage: &pre,
			PaymentAddr:     payAddr,
			Value:           value,
			Features:        defautlInvoiceFeatures,
		},
		Htlcs:    map[models.CircuitKey]*invpkg.InvoiceHTLC{},
		AMPState: map[invpkg.SetID]invpkg.InvoiceStateAMP{},
	}
	i.Memo = []byte("memo")

	// Create a random byte slice of MaxPaymentRequestSize bytes to be used
	// as a dummy paymentrequest, and  determine if it should be set based
	// on one of the random bytes.
	var r [invpkg.MaxPaymentRequestSize]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	if r[0]&1 == 0 {
		i.PaymentRequest = r[:]
	} else {
		i.PaymentRequest = []byte("")
	}

	return i, nil
}

func TestAddInvoice(t *testing.T) {
	t.Parallel()

	ctxt, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	db := NewTestDB(t)

	executor := NewTransactionExecutor(
		db, func(tx *sql.Tx) InvoiceQueries {
			return db.BaseDB.WithTx(tx)
		},
	)

	store := NewInvoiceStore(executor)

	invAmt := lnwire.MilliSatoshi(1000)

	// Check that we can add a bolt11 invoice.
	inv, err := randInvoice(invAmt)
	require.NoError(t, err)

	hash := inv.Terms.PaymentPreimage.Hash()

	err = store.AddInvoice(ctxt, inv, hash)
	require.NoError(t, err)

	require.EqualValues(t, inv.AddIndex, 1)

	ref := invpkg.InvoiceRefByHash(hash)
	dbInv, err := store.LookupInvoice(ctxt, ref)
	require.NoError(t, err)
	require.Equal(t, inv, dbInv)
}

// TestInvoiceTimeSeries tests that newly added invoices invoices, as well as
// settled invoices are added to the database are properly placed in the add
// add or settle index which serves as an event time series.
func TestInvoiceAddTimeSeries(t *testing.T) {
	t.Parallel()

	ctxt, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	db := NewTestDB(t)

	executor := NewTransactionExecutor(
		db, func(tx *sql.Tx) InvoiceQueries {
			return db.BaseDB.WithTx(tx)
		},
	)

	store := NewInvoiceStore(executor)

	_, err := store.InvoicesAddedSince(ctxt, 0)
	require.NoError(t, err)

	// We'll start off by creating 20 random invoices, and inserting them
	// into the database.
	const numInvoices = 20
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]*invpkg.Invoice, numInvoices)
	for i := 0; i < len(invoices); i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		paymentHash := invoice.Terms.PaymentPreimage.Hash()

		err = store.AddInvoice(ctxt, invoice, paymentHash)
		if err != nil {
			t.Fatalf("unable to add invoice %v", err)
		}

		invoices[i] = invoice
	}

	// With the invoices constructed, we'll now create a series of queries
	// that we'll use to assert expected return values of
	// InvoicesAddedSince.
	addQueries := []struct {
		sinceAddIndex uint64

		resp []*invpkg.Invoice
	}{
		// If we specify a value of zero, we shouldn't get any invoices
		// back.
		{
			sinceAddIndex: 0,
		},

		// If we specify a value well beyond the number of inserted
		// invoices, we shouldn't get any invoices back.
		{
			sinceAddIndex: 99999999,
		},

		// Using an index of 1 should result in all values, but the
		// first one being returned.
		{
			sinceAddIndex: 1,
			resp:          invoices[1:],
		},

		// If we use an index of 10, then we should retrieve the
		// reaming 10 invoices.
		{
			sinceAddIndex: 10,
			resp:          invoices[10:],
		},
	}

	for i, query := range addQueries {
		resp, err := store.InvoicesAddedSince(ctxt, query.sinceAddIndex)
		if err != nil {
			t.Fatalf("unable to query: %v", err)
		}

		require.Equal(
			t, len(query.resp), len(resp),
			fmt.Sprintf("test: #%v", i),
		)

		require.Equal(t, len(query.resp), len(resp))

		for j := 0; j < len(query.resp); j++ {
			require.Equal(t,
				query.resp[j], resp[j],
				fmt.Sprintf("test: #%v, item: #%v", i, j),
			)
		}
	}
}
