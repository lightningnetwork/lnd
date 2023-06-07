package sqldb

import (
	"context"
	"crypto/rand"
	"database/sql"
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
